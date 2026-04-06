import { Routes, Route, Navigate } from 'react-router-dom'
import { AuthProvider, useAuth } from './context/AuthContext'
import Layout from './components/Layout'
import LoginPage from './pages/LoginPage'
import SignupPage from './pages/SignupPage'
import LobbyPage from './pages/LobbyPage'
import MyRoomsPage from './pages/MyRoomsPage'
import ChatPage from './pages/ChatPage'
import type { ReactNode } from 'react'

function RequireAuth({ children }: { children: ReactNode }) {
  const { isLoggedIn } = useAuth()
  if (!isLoggedIn) return <Navigate to="/login" replace />
  return <>{children}</>
}

function RedirectIfAuth({ children }: { children: ReactNode }) {
  const { isLoggedIn } = useAuth()
  if (isLoggedIn) return <Navigate to="/lobby" replace />
  return <>{children}</>
}

export default function App() {
  return (
    <AuthProvider>
      <Routes>
        <Route path="/login" element={<RedirectIfAuth><LoginPage /></RedirectIfAuth>} />
        <Route path="/signup" element={<RedirectIfAuth><SignupPage /></RedirectIfAuth>} />
        <Route element={<RequireAuth><Layout /></RequireAuth>}>
          <Route path="/lobby" element={<LobbyPage />} />
          <Route path="/rooms" element={<MyRoomsPage />} />
        </Route>
        <Route path="/chat/:roomId" element={<RequireAuth><ChatPage /></RequireAuth>} />
        <Route path="*" element={<Navigate to="/lobby" replace />} />
      </Routes>
    </AuthProvider>
  )
}
