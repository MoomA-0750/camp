import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { BrowserRouter, NavLink, Navigate, Route, Routes } from 'react-router-dom'
import { api } from './api'
import Sessions from './pages/Sessions'
import SessionDetail from './pages/SessionDetail'
import Search from './pages/Search'
import Usage from './pages/Usage'
import Notes from './pages/Notes'
import NoteDetail from './pages/NoteDetail'
import VaultHealth from './pages/VaultHealth'
import './styles.css'

// ルーティングは本物のURLに乗せる（BrowserRouter）。
// ブックマークも戻る/進むもそのまま効く。深いURLを直接開いても
// 404にならないのは、サーバー側の catch-all が殻を返すから（D-020）。
function App() {
  return (
    <div className="app">
      <nav>
        <h1>Camp</h1>
        <NavLink to="/sessions" className={({ isActive }) => (isActive ? 'on' : '')}>
          セッション
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
        <NavLink to="/vault" className={({ isActive }) => (isActive ? 'on' : '')}>
          Vault の点検
        </NavLink>
        <div className="spacer" />
        <button onClick={() => void api.logout()}>ログアウト</button>
      </nav>
      <main>
        <Routes>
          <Route path="/" element={<Navigate to="/sessions" replace />} />
          <Route path="/sessions" element={<Sessions />} />
          <Route path="/sessions/:id" element={<SessionDetail />} />
          <Route path="/search" element={<Search />} />
          <Route path="/usage" element={<Usage />} />
          <Route path="/notes" element={<Notes />} />
          <Route path="/notes/:id" element={<NoteDetail />} />
          <Route path="/vault" element={<VaultHealth />} />
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
