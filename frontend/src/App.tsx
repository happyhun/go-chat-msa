import { Routes, Route, Navigate } from 'react-router-dom'
import { AuthProvider } from './context/AuthContext'
import { useAuth } from './context/auth'
import Layout from './components/Layout'
import LoginPage from './pages/LoginPage'
import SignupPage from './pages/SignupPage'
import LobbyPage from './pages/LobbyPage'
import MyRoomsPage from './pages/MyRoomsPage'
import ChatPage from './pages/ChatPage'
import type { ReactNode } from 'react'

function RequireAuth({ children }: { children: ReactNode }) {
  const { isLoggedIn, initializing } = useAuth()
  if (initializing) return <AuthLoading />
  if (!isLoggedIn) return <Navigate to="/login" replace />
  return <>{children}</>
}

function RedirectIfAuth({ children }: { children: ReactNode }) {
  const { isLoggedIn, initializing } = useAuth()
  if (initializing) return <AuthLoading />
  if (isLoggedIn) return <Navigate to="/lobby" replace />
  return <>{children}</>
}

function AuthLoading() {
  return (
    <div className="min-h-screen flex items-center justify-center bg-gray-50 px-4">
      <div role="status" aria-live="polite" className="text-sm text-gray-500">
        세션을 확인하는 중...
      </div>
    </div>
  )
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
