import { useEffect, useState } from 'react'
import { autoSaveDirtyChapterTabs } from '../../stores/editor-store'
import { ipc } from '../../services/ipc-client'

/** 章节自动保存的默认间隔（秒）：与主进程全局配置里的默认值一致。 */
export const DEFAULT_AUTO_SAVE_INTERVAL_SECONDS = 30

/** 配置里的间隔只接受有限正数秒；缺失、非数字或非正数时用默认值。 */
export function resolveAutoSaveIntervalSeconds(value: unknown): number {
  return typeof value === 'number' && Number.isFinite(value) && value > 0
    ? value
    : DEFAULT_AUTO_SAVE_INTERVAL_SECONDS
}

/**
 * 正文自动保存：按全局配置的 autoSaveInterval 秒起一个定时器，每一轮把脏的、可写的章节标签交给它登记的保存路径。
 * 一轮没跑完（或某个标签已有保存在途）就不再起新的一轮，失败由保存自己提示，定时器只保证后面的轮次照常。
 */
export function useChapterAutoSave(): void {
  const [intervalSeconds, setIntervalSeconds] = useState(DEFAULT_AUTO_SAVE_INTERVAL_SECONDS)

  useEffect(() => {
    let cancelled = false
    void ipc.invoke('config:get')
      .then(config => {
        if (!cancelled) setIntervalSeconds(resolveAutoSaveIntervalSeconds(config?.autoSaveInterval))
      })
      .catch(() => {
        // 读不到配置时按默认间隔自动保存。
      })
    return () => { cancelled = true }
  }, [])

  useEffect(() => {
    let stopped = false
    let running = false
    const tick = async () => {
      if (running || stopped) return
      running = true
      try {
        await autoSaveDirtyChapterTabs()
      } catch {
        // 单轮异常不影响下一轮。
      } finally {
        running = false
      }
    }
    const timer = window.setInterval(() => { void tick() }, intervalSeconds * 1000)
    return () => {
      stopped = true
      window.clearInterval(timer)
    }
  }, [intervalSeconds])
}
