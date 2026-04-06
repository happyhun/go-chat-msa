import { Outlet, NavLink, useNavigate } from 'react-router-dom'
import { logout } from '../api/client'
import { useAuth } from '../context/AuthContext'

export default function Layout() {
  const { username, doLogout } = useAuth()
  const navigate = useNavigate()

  const handleLogout = async () => {
    try {
      await logout()
    } catch {
      // ignore
    }
    doLogout()
    navigate('/login')
  }

  return (
    <div className="h-screen flex flex-col bg-gray-50">
      {/* Header */}
      <header className="bg-white border-b border-gray-200 shrink-0">
        <div className="max-w-2xl mx-auto px-4 h-14 flex items-center justify-between">
          <h1 className="text-lg font-bold text-gray-900">Go Chat</h1>
          <div className="flex items-center gap-3">
            <span className="text-sm text-gray-500">{username}</span>
            <button
              onClick={handleLogout}
              className="text-sm text-gray-500 hover:text-red-500 transition-colors"
            >
              로그아웃
            </button>
          </div>
        </div>
      </header>

      {/* Tab Navigation */}
      <nav className="bg-white border-b border-gray-200 shrink-0">
        <div className="max-w-2xl mx-auto px-4 flex">
          <NavLink
            to="/lobby"
            className={({ isActive }) =>
              `flex-1 text-center py-3 text-sm font-medium border-b-2 transition-colors ${
                isActive
                  ? 'border-indigo-600 text-indigo-600'
                  : 'border-transparent text-gray-500 hover:text-gray-700'
              }`
            }
          >
            로비
          </NavLink>
          <NavLink
            to="/rooms"
            className={({ isActive }) =>
              `flex-1 text-center py-3 text-sm font-medium border-b-2 transition-colors ${
                isActive
                  ? 'border-indigo-600 text-indigo-600'
                  : 'border-transparent text-gray-500 hover:text-gray-700'
              }`
            }
          >
            내 채팅방
          </NavLink>
        </div>
      </nav>

      {/* Page Content */}
      <main className="flex-1 overflow-y-auto">
        <Outlet />
      </main>
    </div>
  )
}
