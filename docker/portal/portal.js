'use strict';
const $ = id => document.getElementById(id);
function status(message, error = false) {
  const box = $('status'); box.textContent = message; box.hidden = false;
  box.classList.toggle('error', error);
}
async function request(url, options) {
  const response = await fetch(url, {credentials: 'same-origin', ...options});
  const data = await response.json();
  if (!response.ok) throw new Error(data.error || `HTTP ${response.status}`);
  return data;
}
async function action(button, work) {
  button.disabled = true;
  try { await work(); } catch (error) { status(error.message, true); }
  finally { button.disabled = false; }
}
async function refresh() {
  const files = await request('/api/files');
  for (const area of ['imports', 'exports']) {
    $(area).replaceChildren();
    for (const file of files[area]) {
      const li = document.createElement('li'); const link = document.createElement('a');
      link.href = file.url; link.download = file.name;
      link.textContent = `${file.name} (${(file.size / 1024).toFixed(1)} KB)`;
      li.append(link); $(area).append(li);
    }
    if (!files[area].length) { const li = document.createElement('li'); li.textContent = '暂无文件'; $(area).append(li); }
  }
  if (files.truncated) status('文件较多，列表只显示部分文件。');
}
for (const button of document.querySelectorAll('[data-panel]')) {
  button.addEventListener('click', () => {
    const panel = $(button.dataset.panel); panel.hidden = !panel.hidden;
    if (button.dataset.panel === 'transfer' && !panel.hidden) action(button, refresh);
  });
}
$('copy').addEventListener('click', event => action(event.currentTarget, async () => {
  await request('/api/clipboard', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({text: $('text').value})});
  status('已发送到服务器剪贴板。点击桌面内的目标输入框，按 Ctrl+V 粘贴。');
}));
$('upload').addEventListener('click', event => action(event.currentTarget, async () => {
  const file = $('upload-file').files[0];
  if (!file) throw new Error('请先选择要上传的文件。');
  if (file.size > 64 * 1024 * 1024) throw new Error('文件超过 64 MB；请通过服务器共享目录或 SSH 传入。');
  const data = await request('/api/upload?name=' + encodeURIComponent(file.name), {method: 'POST', headers: {'Content-Type': 'application/octet-stream'}, body: file});
  status('上传完成：' + data.path + '。请在应用导入对话框中选择该文件。'); await refresh();
}));
$('refresh').addEventListener('click', event => action(event.currentTarget, refresh));
