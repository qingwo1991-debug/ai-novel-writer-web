# 书童 Pagekin (AI-Novel-Writer) 局域网 Web 部署镜像
# 架构：完整 Electron 桌面应用 + Xvfb 虚拟显示 + x11vnc + noVNC(websockify)
# 浏览器访问 http://<服务器IP>:6080/vnc.html 即可操作完整应用
FROM node:22-bookworm-slim

ENV DEBIAN_FRONTEND=noninteractive \
    LANG=C.UTF-8 \
    npm_config_registry=https://registry.npmmirror.com \
    ELECTRON_MIRROR=https://registry.npmmirror.com/-/binary/electron/ \
    ELECTRON_SKIP_BINARY_DOWNLOAD=0

# 系统依赖：
#  - Electron/Chromium 运行库
#  - Xvfb/x11vnc/novnc：虚拟桌面 + VNC
#  - dbus-x11：会话总线
#  - 中文字体（界面中文显示必须）
#  - 构建工具：better-sqlite3 / lancedb 原生模块编译
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl git \
      xvfb x11vnc websockify novnc dbus-x11 xauth \
      fonts-noto-cjk fonts-noto-color-emoji \
      libgtk-3-0 libnss3 libasound2 libxss1 libatk-bridge2.0-0 \
      libdrm2 libgbm1 libxcomposite1 libxdamage1 libxfixes3 \
      libxrandr2 libxtst6 libcups2 libxkbcommon0 libpango-1.0-0 \
      libcairo2 libglib2.0-0 libgdk-pixbuf-2.0-0 libx11-xcb1 \
      libxcb-dri3-0 libpulse0 \
      build-essential python3 pkg-config \
    && rm -rf /var/lib/apt/lists/*

# 固定 pnpm 版本（与仓库 packageManager 一致）
RUN corepack enable && corepack prepare pnpm@11.11.0 --activate

WORKDIR /app

# 先装依赖（利用层缓存）
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml .npmrc ./
RUN pnpm install --frozen-lockfile

# 拷贝源码并构建（tsc + vite build -> dist/ 与 dist-electron/）
COPY . .
RUN pnpm build

# Browser-side Chinese clipboard and server-side transfer portal.
RUN apt-get update && apt-get install -y --no-install-recommends xclip xdotool \
    && rm -rf /var/lib/apt/lists/*
EXPOSE 6080

COPY docker/bootstrap.py /opt/novel-docker/bootstrap.py
COPY docker/portal/ /opt/novel-portal/
COPY docker/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# 数据卷：userData(配置+SQLite) 与作品文件夹都落在 /data，宿主挂载即持久化
VOLUME ["/data"]

ENTRYPOINT ["/entrypoint.sh"]