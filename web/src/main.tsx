import { StrictMode, Suspense, lazy, useCallback, useState } from 'react'
import { createRoot } from 'react-dom/client'
import { BrowserRouter, NavLink, Navigate, Route, Routes } from 'react-router-dom'
import { api } from './api'
import Switcher, { useSwitcherKey } from './Switcher'
import NoteNew from './pages/NoteNew'
import Sessions from './pages/Sessions'
import Runtime from './pages/Runtime'
import RuntimeDetail from './pages/RuntimeDetail'
import SessionDetail from './pages/SessionDetail'
import Search from './pages/Search'
import Usage from './pages/Usage'
import Notes from './pages/Notes'
import NoteDetail from './pages/NoteDetail'
import VaultHealth from './pages/VaultHealth'
import Audit from './pages/Audit'
import Views from './pages/Views'
import ViewDetail from './pages/ViewDetail'
import ViewDefEdit from './pages/ViewDefEdit'
import Graph from './pages/Graph'
import { clearAll as clearDrafts } from './editor/drafts'

// 書く画面は CodeMirror を抱えるので遅れて読む（ほかの画面の読み込みを重くしない）。
const NoteEdit = lazy(() => import('./pages/NoteEdit'))
import './styles.css'

// ルーティングは本物のURLに乗せる（BrowserRouter）。
// ブックマークも戻る/進むもそのまま効く。深いURLを直接開いても
// 404にならないのは、サーバー側の catch-all が殻を返すから（D-020）。
function App() {
  // クイックスイッチャー（Ctrl+O・⌘O・ナビの「開く」）。Phase 5 / M55。
  const [switcher, setSwitcher] = useState(false)
  const openSwitcher = useCallback(() => setSwitcher(true), [])
  useSwitcherKey(openSwitcher)
  return (
    <div className="app">
      {switcher && <Switcher onClose={() => setSwitcher(false)} />}
      <nav>
        <h1>Camp</h1>
        <NavLink to="/sessions" className={({ isActive }) => (isActive ? 'on' : '')}>
          セッション
        </NavLink>
        <NavLink to="/runtime" className={({ isActive }) => (isActive ? 'on' : '')}>
          駆動
        </NavLink>
        <NavLink to="/search" className={({ isActive }) => (isActive ? 'on' : '')}>
          検索
        </NavLink>
        <NavLink to="/usage" className={({ isActive }) => (isActive ? 'on' : '')}>
          使用量
        </NavLink>
        <NavLink to="/notes" className={({ isActive }) => (isActive ? 'on' : '')}>
          ノート
        </NavLink>
        <NavLink to="/views" className={({ isActive }) => (isActive ? 'on' : '')}>
          ビュー
        </NavLink>
        <NavLink to="/graph" className={({ isActive }) => (isActive ? 'on' : '')}>
          グラフ
        </NavLink>
        <NavLink to="/vault" className={({ isActive }) => (isActive ? 'on' : '')}>
          Vault の点検
        </NavLink>
        <NavLink to="/audit" className={({ isActive }) => (isActive ? 'on' : '')}>
          監査ログ
        </NavLink>
        <button onClick={openSwitcher} title="Ctrl+O">開く</button>
        <div className="spacer" />
        <button onClick={() => { clearDrafts(); void api.logout() }}>ログアウト</button>
      </nav>
      <main>
        <Routes>
          <Route path="/" element={<Navigate to="/sessions" replace />} />
          <Route path="/sessions" element={<Sessions />} />
          <Route path="/sessions/:id" element={<SessionDetail />} />
          <Route path="/runtime" element={<Runtime />} />
          <Route path="/runtime/:id" element={<RuntimeDetail />} />
          <Route path="/search" element={<Search />} />
          <Route path="/usage" element={<Usage />} />
          <Route path="/notes" element={<Notes />} />
          <Route path="/notes/new" element={<NoteNew />} />
          <Route path="/notes/:id" element={<NoteDetail />} />
          <Route path="/notes/:id/edit" element={<Suspense fallback={<p className="muted">読み込み中…</p>}><NoteEdit /></Suspense>} />
          <Route path="/views" element={<Views />} />
          {/* 定義の編集は `/views/` の下に置かない——`/views/*` が詳細の catch-all なので
              飲まれる（2026-09-13）。独立させれば `def` という名前の台紙でも壊れない。 */}
          <Route path="/viewdefs/:base" element={<ViewDefEdit />} />
          <Route path="/views/*" element={<ViewDetail />} />
          <Route path="/graph" element={<Graph />} />
          <Route path="/vault" element={<VaultHealth />} />
          <Route path="/audit" element={<Audit />} />
          <Route path="*" element={<NotFound />} />
        </Routes>
      </main>
    </div>
  )
}

function NotFound() {
  return (
    <>
      <h2>そのページは無い</h2>
      <p className="sub muted">
        URL は合っているのに出ないなら、まだ作っていない画面かもしれない。
      </p>
    </>
  )
}

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <BrowserRouter>
      <App />
    </BrowserRouter>
  </StrictMode>,
)
