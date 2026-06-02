import { useState } from 'react'
import { Outlet, NavLink, useNavigate } from 'react-router-dom'
import { deleteUser, logout } from '../api/client'
import { useAuth } from '../context/auth'
import { useToast } from '../context/toast'
import DeleteUserModal from './DeleteUserModal'
import UserMenu from './UserMenu'

export default function Layout() {
  const { username, doLogout } = useAuth()
  const { success } = useToast()
  const navigate = useNavigate()
  const [deleteOpen, setDeleteOpen] = useState(false)

  const handleLogout = async () => {
    try {
      await logout()
    } catch {
      // ignore
    }
    doLogout()
    navigate('/login', { replace: true })
  }

  const handleDeleteUser = async (password: string) => {
    await deleteUser(password)
    setDeleteOpen(false)
    doLogout()
    success('탈퇴가 완료되었습니다.')
    navigate('/login', { replace: true })
  }

  return (
    <div className="h-screen flex flex-col bg-gray-50">
      {/* Header */}
      <header className="bg-white border-b border-gray-200 shrink-0">
        <div className="max-w-2xl mx-auto px-4 h-14 flex items-center justify-between">
          <h1 className="text-lg font-bold text-gray-900">Go Chat</h1>
          {username && (
            <UserMenu
              username={username}
              onLogout={handleLogout}
              onDeleteUser={() => setDeleteOpen(true)}
            />
          )}
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

      {deleteOpen && (
        <DeleteUserModal
          onConfirm={handleDeleteUser}
          onCancel={() => setDeleteOpen(false)}
        />
      )}
    </div>
  )
}
