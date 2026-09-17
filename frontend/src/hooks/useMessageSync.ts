import { useCallback, useEffect, useRef, useState, type RefObject } from 'react'
import { ApiError, listMessages } from '../api/client'
import { rewindMessageId, lastMessageId, oldestRecoveryAnchor } from '../messages'
import { jitteredBackoff } from '../retry'
import type { MessageInfo } from '../types'

export function useMessageSync(roomId: string, userId: string, lastId: RefObject<string>, onMessages: (messages: MessageInfo[]) => void) {
  const key = `chat:recovery:${userId}:${roomId}`
  const anchor = useRef<string | null>(null)
  const deadline = useRef(0)
  const attempt = useRef(0)
  const lastError = useRef(false)
  const busy = useRef(false)
  const controller = useRef<AbortController | null>(null)
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)
  const runRef = useRef<(recent?: boolean) => Promise<void>>(async () => {})
  const callback = useRef(onMessages)
  const [exhausted, setExhausted] = useState(false)
  useEffect(() => { callback.current = onMessages }, [onMessages])

  const run = useCallback(async (recent = false) => {
    if (!controller.current) return
    if (!recent && Date.now() >= deadline.current) {
      setExhausted(lastError.current)
      return
    }
    if (busy.current) {
      if (!recent) {
        clearTimeout(timer.current)
        timer.current = setTimeout(() => { void runRef.current() }, 250)
      }
      return
    }
    const active = controller.current
    const windowSignal = AbortSignal.timeout(recent ? 5000 : Math.max(1, deadline.current - Date.now()))
    busy.current = true
    try {
      let cursor = recent ? rewindMessageId(lastId.current) : anchor.current ?? rewindMessageId(lastId.current)
      for (;;) {
        const response = await listMessages(roomId, cursor, 1000, AbortSignal.any([active.signal, windowSignal, AbortSignal.timeout(5000)]))
        if (active.signal.aborted) return
        const messages = response.messages ?? []
        callback.current(messages)
        if (!response.has_more || messages.length === 0) break
        cursor = lastMessageId(messages)
      }
      lastError.current = false
    } catch (err) {
      if (active.signal.aborted) return
      lastError.current = true
      if (err instanceof ApiError && [401, 403, 404].includes(err.status)) return
      console.warn('message sync retry:', err)
    } finally {
      if (controller.current === active) busy.current = false
      if (!recent && !active.signal.aborted && anchor.current !== null) {
        const remaining = deadline.current - Date.now()
        clearTimeout(timer.current)
        if (remaining <= 0) setExhausted(lastError.current)
        else timer.current = setTimeout(() => { void runRef.current() }, Math.min(remaining, jitteredBackoff(attempt.current++)))
      }
    }
  }, [roomId, lastId])
  useEffect(() => { runRef.current = run }, [run])

  const recover = useCallback((fromId = lastId.current) => {
    anchor.current = oldestRecoveryAnchor(anchor.current, rewindMessageId(fromId))
    sessionStorage.setItem(key, anchor.current)
    if (deadline.current <= Date.now()) {
      deadline.current = Date.now() + 60000
      attempt.current = 0
    }
    setExhausted(false)
    lastError.current = false
    clearTimeout(timer.current)
    void runRef.current()
  }, [key, lastId])

  useEffect(() => {
    const active = new AbortController()
    controller.current = active
    busy.current = false
    anchor.current = sessionStorage.getItem(key)
    deadline.current = Date.now() + 60000
    const periodic = setInterval(() => {
      void runRef.current(true)
    }, 30000)
    const focus = () => recover()
    window.addEventListener('focus', focus)
    if (anchor.current !== null) void runRef.current()
    return () => { active.abort(); clearTimeout(timer.current); clearInterval(periodic); window.removeEventListener('focus', focus) }
  }, [key, recover])

  return { recover, exhausted }
}
