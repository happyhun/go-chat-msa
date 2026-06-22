import { useId, useState } from 'react'
import { useNavigate, Link } from 'react-router-dom'
import { signup, login, ApiError } from '../api/client'
import { useAuth } from '../context/auth'

const usernamePattern = /^[a-zA-Z0-9가-힣]+$/u

function isValidSignupPassword(value: string) {
  const classes = [
    /[a-z]/.test(value),
    /[A-Z]/.test(value),
    /\d/.test(value),
    /[^a-zA-Z\d]/.test(value),
  ].filter(Boolean).length
  return value.length >= 10 && value.length <= 64 && classes >= 3
}

export default function SignupPage() {
  const usernameId = useId()
  const usernameHelpId = useId()
  const passwordId = useId()
  const passwordHelpId = useId()
  const errorId = useId()
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)
  const navigate = useNavigate()
  const { doLogin } = useAuth()

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    const userName = username.trim()
    if (!userName) {
      setError('사용자 이름을 입력해 주세요.')
      return
    }
    if (userName.length < 2 || userName.length > 12 || !usernamePattern.test(userName)) {
      setError('사용자 이름은 영문, 숫자, 한글 2~12자로 입력해 주세요.')
      return
    }
    if (!isValidSignupPassword(password)) {
      setError('비밀번호 조건을 확인해 주세요.')
      return
    }
    setError('')
    setLoading(true)
    try {
      await signup(userName, password)
      try {
        const res = await login(userName, password)
        doLogin(res.access_token, res.user_id, userName)
        navigate('/lobby')
      } catch {
        navigate('/login', {
          replace: true,
          state: {
            username: userName,
            notice: '회원가입은 완료되었습니다. 로그인해 주세요.',
          },
        })
      }
    } catch (err) {
      if (err instanceof ApiError) setError(err.message)
      else setError('회원가입에 실패했습니다.')
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="min-h-screen flex items-center justify-center bg-gray-50 px-4">
      <div className="w-full max-w-sm">
        <div className="text-center mb-8">
          <div className="w-14 h-14 bg-indigo-600 rounded-lg flex items-center justify-center mx-auto mb-4">
            <svg className="w-7 h-7 text-white" fill="none" viewBox="0 0 24 24" stroke="currentColor">
              <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M18 9v3m0 0v3m0-3h3m-3 0h-3m-2-5a4 4 0 11-8 0 4 4 0 018 0zM3 20a6 6 0 0112 0v1H3v-1z" />
            </svg>
          </div>
          <h1 className="text-2xl font-bold text-gray-900">회원가입</h1>
          <p className="text-sm text-gray-500 mt-1">Go Chat에 오신 것을 환영합니다</p>
        </div>

        <div className="bg-white rounded-lg shadow-sm border border-gray-100 p-6">
          <form onSubmit={handleSubmit} className="space-y-4">
            <div>
              <label htmlFor={usernameId} className="block text-sm font-medium text-gray-700 mb-1">사용자 이름</label>
              <input
                id={usernameId}
                type="text"
                value={username}
                onChange={(e) => setUsername(e.target.value)}
                autoComplete="username"
                disabled={loading}
                aria-invalid={Boolean(error)}
                aria-describedby={`${usernameHelpId}${error ? ` ${errorId}` : ''}`}
                className="w-full px-3 py-2.5 border border-gray-300 rounded-lg focus:outline-none focus:ring-2 focus:ring-indigo-500 focus:border-transparent text-sm"
                placeholder="사용자 이름"
                required
                minLength={2}
                maxLength={12}
                autoFocus
              />
              <p id={usernameHelpId} className="mt-1 text-xs text-gray-500">
                영문, 숫자, 한글 2~12자
              </p>
            </div>
            <div>
              <label htmlFor={passwordId} className="block text-sm font-medium text-gray-700 mb-1">비밀번호</label>
              <input
                id={passwordId}
                type="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                autoComplete="new-password"
                disabled={loading}
                aria-invalid={Boolean(error)}
                aria-describedby={`${passwordHelpId}${error ? ` ${errorId}` : ''}`}
                className="w-full px-3 py-2.5 border border-gray-300 rounded-lg focus:outline-none focus:ring-2 focus:ring-indigo-500 focus:border-transparent text-sm"
                placeholder="비밀번호"
                required
                minLength={10}
                maxLength={64}
              />
              <p id={passwordHelpId} className="mt-1 text-xs text-gray-500">
                대문자, 소문자, 숫자, 특수문자 중 3종류 이상 조합, 10자 이상
              </p>
            </div>
            {error && <p id={errorId} role="alert" aria-live="polite" className="text-red-500 text-sm">{error}</p>}
            <button
              type="submit"
              disabled={loading}
              className="w-full py-2.5 bg-indigo-600 text-white rounded-lg font-medium hover:bg-indigo-700 disabled:opacity-50 transition-colors text-sm"
            >
              {loading ? '가입 중...' : '가입하기'}
            </button>
          </form>
        </div>

        <p className="mt-6 text-center text-sm text-gray-500">
          이미 계정이 있으신가요?{' '}
          <Link to="/login" className="text-indigo-600 hover:text-indigo-700 font-medium">
            로그인
          </Link>
        </p>
      </div>
    </div>
  )
}
