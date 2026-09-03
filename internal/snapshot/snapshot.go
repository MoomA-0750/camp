// Package snapshot は DB の一貫したスナップショットを取り、暗号化して書き出す。
//
// **campd は鍵を持たない。** 鍵は 1Password に置き、取るときと戻すときだけ
// 標準入力から渡す。稼働中のDBを暗号化しない（SQLCipher は systemd 常駐と
// 相性が悪く、再起動のたびに手で解錠することになる）と決めた以上、
// 守れるのは退避先だけなので、そこは確実に守る。
package snapshot

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/MoomA-0750/camp/internal/store"
)

// ファイル形式:
//
//	"CAMPSNAP" | version(1) | iterations(4) | salt(16) | noncePrefix(4)
//	→ 以降 [ciphertext(chunk + GCMタグ)] の繰り返し
//
// nonce は noncePrefix(4) + 連番(8)。連番が違えば nonce も違うので使い回さない。
// AAD に連番と「最後のかたまりか」を入れる。**入れないと、後ろを切り落とした
// ファイルが正しい復号として通ってしまう**（静かに欠けたバックアップになる）。
const (
	magic      = "CAMPSNAP"
	version    = 1
	saltLen    = 16
	prefixLen  = 4
	nonceLen   = 12
	keyLen     = 32
	chunkSize  = 4 << 20 // 4 MiB。274 MB を丸ごとメモリに載せない
	iterations = 600_000 // OWASP の PBKDF2-HMAC-SHA256 推奨値
	headerLen  = len(magic) + 1 + 4 + saltLen + prefixLen
)

// Info は取った／戻したスナップショットの素性。
type Info struct {
	Path        string
	PlainBytes  int64  // 暗号化前のバイト数
	CipherBytes int64  // 書き出したファイルのバイト数
	SHA256      string // 暗号化前の中身のハッシュ。戻したものと突き合わせる
}

// Create は DB の一貫したスナップショットを取り、暗号化して out に書く。
//
// `VACUUM INTO` を使う。稼働中でも WAL ごと辻褄の合った1ファイルになるので、
// ファイルをコピーする方式（-wal と -shm を取りこぼす）より確実。
func Create(db *store.DB, out string, key []byte) (Info, error) {
	var info Info
	if len(key) == 0 {
		return info, errors.New("鍵が空。1Password から渡す")
	}
	if _, err := os.Stat(out); err == nil {
		return info, fmt.Errorf("%s は既にある。上書きしない", out)
	}

	// **平文の中間ファイルを、他人が読める場所に置かない。**
	// VACUUM INTO が作るファイルの mode は umask 次第で、実測（2026-09-03）では
	// 0644 だった。暗号文だけ 0600 にしても、暗号化している数秒のあいだ
	// 264MB の平文が誰でも読める状態で置かれる（M23 が引いた線をそこで越える）。
	// ファイル自身の mode は VACUUM INTO に指定できないので、**0700 の
	// 専用ディレクトリの中に閉じ込める。**
	tmpDir, err := os.MkdirTemp(filepath.Dir(out), ".camp-snap-")
	if err != nil {
		return info, err
	}
	if err := os.Chmod(tmpDir, 0o700); err != nil {
		os.RemoveAll(tmpDir)
		return info, err
	}
	defer os.RemoveAll(tmpDir)
	tmp := filepath.Join(tmpDir, "plain.sqlite")
	if _, err := db.Exec(`VACUUM INTO ?`, tmp); err != nil {
		return info, fmt.Errorf("スナップショットを取れない: %w", err)
	}
	// 念のため、ファイル自体も落としておく（ディレクトリと二重に守る）。
	_ = os.Chmod(tmp, 0o600)

	src, err := os.Open(tmp)
	if err != nil {
		return info, err
	}
	defer src.Close()

	dst, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return info, err
	}
	defer dst.Close()

	sum := sha256.New()
	n, err := encrypt(io.TeeReader(src, sum), dst, key)
	if err != nil {
		os.Remove(out)
		return info, err
	}
	if err := dst.Sync(); err != nil {
		return info, err
	}

	st, _ := os.Stat(out)
	pst, _ := os.Stat(tmp)
	info = Info{Path: out, PlainBytes: pst.Size(), CipherBytes: st.Size(),
		SHA256: hex.EncodeToString(sum.Sum(nil))}
	_ = n
	return info, nil
}

// Restore は暗号化されたスナップショットを out へ戻す。
func Restore(in, out string, key []byte) (Info, error) {
	var info Info
	if len(key) == 0 {
		return info, errors.New("鍵が空。1Password から渡す")
	}
	if _, err := os.Stat(out); err == nil {
		return info, fmt.Errorf("%s は既にある。上書きしない", out)
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return info, err
	}

	src, err := os.Open(in)
	if err != nil {
		return info, err
	}
	defer src.Close()
	dst, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return info, err
	}
	defer dst.Close()

	sum := sha256.New()
	n, err := decrypt(src, io.MultiWriter(dst, sum), key)
	if err != nil {
		dst.Close()
		os.Remove(out) // 中途半端なDBを残さない
		return info, err
	}
	if err := dst.Sync(); err != nil {
		return info, err
	}
	cst, _ := os.Stat(in)
	return Info{Path: out, PlainBytes: n, CipherBytes: cst.Size(),
		SHA256: hex.EncodeToString(sum.Sum(nil))}, nil
}

func encrypt(r io.Reader, w io.Writer, key []byte) (int64, error) {
	return encryptWith(r, w, key, iterations)
}

// encryptWith は回数を指定して暗号化する。テストが 600,000 回を毎回回さずに
// 済むようにしてあるだけで、本番の経路は必ず iterations を通る。
func encryptWith(r io.Reader, w io.Writer, key []byte, iter int) (int64, error) {
	salt := make([]byte, saltLen)
	prefix := make([]byte, prefixLen)
	if _, err := rand.Read(salt); err != nil {
		return 0, err
	}
	if _, err := rand.Read(prefix); err != nil {
		return 0, err
	}
	gcm, err := newGCMIter(key, salt, iter)
	if err != nil {
		return 0, err
	}

	head := make([]byte, 0, headerLen)
	head = append(head, magic...)
	head = append(head, version)
	head = binary.BigEndian.AppendUint32(head, uint32(iter))
	head = append(head, salt...)
	head = append(head, prefix...)
	if _, err := w.Write(head); err != nil {
		return 0, err
	}

	buf := make([]byte, chunkSize)
	var total int64
	var counter uint64
	for {
		n, readErr := io.ReadFull(r, buf)
		last := readErr == io.EOF || readErr == io.ErrUnexpectedEOF
		if readErr != nil && !last {
			return total, readErr
		}
		if n == 0 && counter > 0 && last {
			// ちょうど区切りで終わった。最後の印だけのかたまりを1つ書く。
			if err := writeChunk(w, gcm, prefix, counter, nil, true); err != nil {
				return total, err
			}
			return total, nil
		}
		if err := writeChunk(w, gcm, prefix, counter, buf[:n], last); err != nil {
			return total, err
		}
		total += int64(n)
		counter++
		if last {
			return total, nil
		}
	}
}

func decrypt(r io.Reader, w io.Writer, key []byte) (int64, error) {
	head := make([]byte, headerLen)
	if _, err := io.ReadFull(r, head); err != nil {
		return 0, fmt.Errorf("見出しを読めない: %w", err)
	}
	if string(head[:len(magic)]) != magic {
		return 0, errors.New("Camp のスナップショットではない")
	}
	if head[len(magic)] != version {
		return 0, fmt.Errorf("知らない版: %d", head[len(magic)])
	}
	iter := binary.BigEndian.Uint32(head[len(magic)+1:])
	salt := head[len(magic)+5 : len(magic)+5+saltLen]
	prefix := head[len(magic)+5+saltLen:]

	gcm, err := newGCMIter(key, salt, int(iter))
	if err != nil {
		return 0, err
	}

	buf := make([]byte, chunkSize+gcm.Overhead())
	var total int64
	var counter uint64
	for {
		n, readErr := io.ReadFull(r, buf)
		if n == 0 {
			return total, errors.New("最後の印が無いまま終わっている。途中で切れたファイル")
		}
		atEOF := readErr == io.EOF || readErr == io.ErrUnexpectedEOF
		if readErr != nil && !atEOF {
			return total, readErr
		}
		// 最後かどうかは AAD に入っているので、両方で開けてみる。
		plain, last, err := openChunk(gcm, prefix, counter, buf[:n])
		if err != nil {
			return total, err
		}
		if _, err := w.Write(plain); err != nil {
			return total, err
		}
		total += int64(len(plain))
		counter++
		if last {
			if !atEOF {
				return total, errors.New("最後の印のあとにデータが続いている")
			}
			return total, nil
		}
		if atEOF {
			return total, errors.New("最後の印が無いまま終わっている。途中で切れたファイル")
		}
	}
}

func writeChunk(w io.Writer, gcm cipher.AEAD, prefix []byte, counter uint64, plain []byte, last bool) error {
	out := gcm.Seal(nil, nonceOf(prefix, counter), plain, aad(counter, last))
	_, err := w.Write(out)
	return err
}

// openChunk は「最後の印つき」「印なし」の両方で開けてみる。
// どちらでも開かなければ、鍵が違うか、中身が書き換わっている。
func openChunk(gcm cipher.AEAD, prefix []byte, counter uint64, ct []byte) ([]byte, bool, error) {
	nonce := nonceOf(prefix, counter)
	if p, err := gcm.Open(nil, nonce, ct, aad(counter, false)); err == nil {
		return p, false, nil
	}
	if p, err := gcm.Open(nil, nonce, ct, aad(counter, true)); err == nil {
		return p, true, nil
	}
	return nil, false, fmt.Errorf("%d 番目のかたまりを開けない。鍵が違うか、中身が書き換わっている", counter)
}

func nonceOf(prefix []byte, counter uint64) []byte {
	n := make([]byte, nonceLen)
	copy(n, prefix)
	binary.BigEndian.PutUint64(n[prefixLen:], counter)
	return n
}

// aad は「何番目のかたまりか」と「最後か」を認証に含める。
// 順番の入れ替えと、後ろの切り落としを検出するため。
func aad(counter uint64, last bool) []byte {
	b := make([]byte, 9)
	binary.BigEndian.PutUint64(b, counter)
	if last {
		b[8] = 1
	}
	return b
}

func newGCMIter(key, salt []byte, iter int) (cipher.AEAD, error) {
	dk, err := pbkdf2.Key(sha256.New, string(key), salt, iter, keyLen)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(dk)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
