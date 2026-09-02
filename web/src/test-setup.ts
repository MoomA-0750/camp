// 画面のテストは fetch を差し替えて動かす。ここで見たいのは
// 「URL がそのまま画面の状態になっているか」であって、API の中身ではない。
import { afterEach } from 'vitest'
import { cleanup } from '@testing-library/react'

afterEach(cleanup)
