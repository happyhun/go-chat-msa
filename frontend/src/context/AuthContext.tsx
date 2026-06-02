import { useState, type ReactNode } from 'react'
import {
  setAuth,
  restoreAuth,
  getCurrentUserId,
  getCurrentUsername,
  getAccessToken,
} from '../api/client'
import { AuthContext } from './auth'

function initAuth() {
  restoreAuth()
  return { userId: getCurrentUserId(), username: getCurrentUsername() }
}

export function AuthProvider({ children }: { children: ReactNode }) {
  const [userId, setUserId] = useState<string | null>(() => initAuth().userId)
  const [username, setUsername] = useState<string | null>(getCurrentUsername)

  const doLogin = (token: string, uid: string, uname: string) => {
    setAuth(token, uid, uname)
    setUserId(uid)
    setUsername(uname)
  }

  const doLogout = () => {
    setAuth(null, null, null)
    setUserId(null)
    setUsername(null)
  }

  return (
    <AuthContext.Provider
      value={{
        userId,
        username,
        isLoggedIn: !!getAccessToken(),
        doLogin,
        doLogout,
      }}
    >
      {children}
    </AuthContext.Provider>
  )
}
