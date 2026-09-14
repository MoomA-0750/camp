import { build } from './mdtree'

// プレビューの木を作る Worker（Phase 5 / M54）。**画面のスレッドで解析しない**——細工した入力や
// 大きなノートで固まっても、書く面は止まらない。画面は時間の上限で打ち切る（useMdTree）。
self.onmessage = (e: MessageEvent<{ id: number; doc: string; origin: string }>) => {
  const { id, doc, origin } = e.data
  try {
    postMessage({ id, tree: build(doc, origin) })
  } catch (err) {
    postMessage({ id, error: String(err) })
  }
}
