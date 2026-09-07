import { expect, test } from 'vitest'
import { act, render, screen, waitFor } from '@testing-library/react'
import { useState } from 'react'
import { useAsync } from './ui'

// **読み直しのあいだ、前の中身を消さない。**
//
// 2026-09-07、セッションの画面が上下に震えた。定期的に読み直すたびに data が
// 消えて、見出しが縮み、そのあいだに別の判断（流すかどうか）が裏返って
// 流れを繋ぎ直し、同じ行をもう一度受け取る——という輪になっていた。
test('読み直しの最中も、前の中身が消えない', async () => {
  let n = 0
  let resolve: ((v: string) => void) | null = null

  function Probe() {
    const [dep, setDep] = useState(0)
    const a = useAsync(() => {
      n++
      if (n === 1) return Promise.resolve('1回目')
      return new Promise<string>((r) => { resolve = r })
    }, [dep])
    return (
      <div>
        <span data-testid="v">{a.data ?? '(空)'}</span>
        <span data-testid="l">{a.loading ? '読み込み中' : '済'}</span>
        <button onClick={() => setDep((v) => v + 1)}>読み直す</button>
      </div>
    )
  }

  render(<Probe />)
  await waitFor(() => expect(screen.getByTestId('v').textContent).toBe('1回目'))

  // 読み直しを始める。**まだ前の中身が出ている。**
  act(() => { screen.getByText('読み直す').click() })
  await waitFor(() => expect(screen.getByTestId('l').textContent).toBe('読み込み中'))
  expect(screen.getByTestId('v').textContent).toBe('1回目')

  await act(async () => { resolve?.('2回目') })
  await waitFor(() => expect(screen.getByTestId('v').textContent).toBe('2回目'))
})
