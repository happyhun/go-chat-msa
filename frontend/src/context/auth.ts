import { createContext, useContext } from 'react'

export interface AuthState {
  userId: string | null
  username: string | null
  isLoggedIn: boolean
  doLogin: (token: string, userId: string, username: string) => void
  doLogout: () => void
}

export const AuthContext = createContext<AuthState>(null!)

export function useAuth() {
  return useContext(AuthContext)
}
