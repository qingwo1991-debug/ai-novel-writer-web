import { ipc } from '../../../services/ipc-client'
import { APP_BRAND } from '../../../shared/brand'
import { useLocaleStore } from '../../../stores/locale-store'
import { UpdateSettings } from '../StageUpdate'

/** 关于与更新：应用名、版本、定位说明，以及检查更新与更新状态。 */
export default function AboutSection() {
  const text = useLocaleStore(s => s.text)
  return (
    <div className="stage-set-rows">
      <div className="stage-set-about">
        <span className="stage-set-about-mark" aria-hidden="true" />
        <div>
          <p className="stage-set-about-name">{text(`${APP_BRAND.zhName} ${APP_BRAND.enName}`, APP_BRAND.enName)}</p>
          <p className="stage-set-about-former">{text(`原名「${APP_BRAND.formerZhName}」`, `Formerly “${APP_BRAND.formerEnName}”`)}</p>
          <p className="stage-set-about-version">{text('版本', 'Version')} <span className="pi-num">{__APP_VERSION__}</span></p>
        </div>
      </div>
      <p className="stage-set-about-text">{ipc.runtimeKind === 'web' ? text(
        '面向长篇小说的原生浏览器写作工具。作品、设定与草稿保存在服务器书库，而不是当前浏览器的电脑上；从设定、架构、蓝图到正文，保留完整创作流程。运行创作任务时请保持页面打开。',
        'A native-browser writing tool for long-form fiction. Books, settings and drafts stay in the server library, not on the browser computer. The complete setup-to-manuscript workflow is retained. Keep the page open while creative tasks run.',
      ) : text(
        '面向长篇小说的本地写作工具。作品、设定与草稿都存在你的电脑上；从设定、架构、蓝图到正文，每一步都可以交给你选择的模型协助。',
        'A local writing tool for long-form fiction. Your books, settings and drafts stay on your computer, and every step from setup to manuscript can be assisted by the models you choose.',
      )}</p>
      <UpdateSettings />
    </div>
  )
}
