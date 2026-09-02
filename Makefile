# ビルドは2段。フロントを先に作ってから Go に埋め込む。
#
# web/dist は git に置かない。node を持たないところでも campd はビルドでき、
# その場合は組み込みの仮の殻（API一覧が出るだけのページ）に落ちる。

.PHONY: all web campd test test-go test-web fmt clean

all: web campd

web:
	cd web && npm ci --silent && npm run build

campd:
	go build -o campd ./cmd/campd

test: test-go test-web

test-go:
	go vet ./...
	gofmt -l . | grep -v '^web/' | (! grep .)
	go test ./...

test-web:
	cd web && npm test

fmt:
	gofmt -w .

clean:
	rm -f campd
	rm -rf web/dist/assets web/dist/index.html
