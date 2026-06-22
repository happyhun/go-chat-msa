import { useEffect, useState, type ReactNode } from 'react'
import {
  setAuth,
  restoreAuth,
  getCurrentUserId,
  getCurrentUsername,
  getAccessToken,
  isAccessTokenExpired,
  refreshAuthSession,
} from '../api/client'
import { AuthContext } from './auth'

function initAuth() {
  restoreAuth()
  return { userId: getCurrentUserId(), username: getCurrentUsername() }
}

export function AuthProvider({ children }: { children: ReactNode }) {
  const [userId, setUserId] = useState<string | null>(() => initAuth().userId)
  const [username, setUsername] = useState<string | null>(getCurrentUsername)
  const [initializing, setInitializing] = useState(true)

  useEffect(() => {
    let cancelled = false

    async function initialize() {
      restoreAuth()
      const token = getAccessToken()
      if (token && !isAccessTokenExpired(token)) {
        if (!cancelled) {
          setUserId(getCurrentUserId())
          setUsername(getCurrentUsername())
          setInitializing(false)
        }
        return
      }

      const session = await refreshAuthSession()
      if (!cancelled) {
        if (session) {
          setUserId(session.userId)
          setUsername(session.username)
        } else {
          setAuth(null, null, null)
          setUserId(null)
          setUsername(null)
        }
        setInitializing(false)
      }
    }

    initialize()

    return () => {
      cancelled = true
    }
  }, [])

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
        isLoggedIn: Boolean(userId && username && getAccessToken()),
        initializing,
        doLogin,
        doLogout,
      }}
    >
      {children}
    </AuthContext.Provider>
  )
}
