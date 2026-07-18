export type Locale = 'en' | 'zh-cn';

type Feature = {
  number: string;
  title: string;
  description: string;
  command: string;
  span?: 'wide';
};

type Step = {
  label: string;
  title: string;
  description: string;
  command: string;
};

export type Copy = {
  meta: { title: string; description: string };
  nav: { features: string; workflow: string; integrations: string; github: string; language: string };
  hero: {
    badge: string;
    line1: string;
    line2: string;
    description: string;
    install: string;
    copy: string;
    copied: string;
    github: string;
    note: string;
    signals: string[];
    demoAlt: string;
  };
  features: {
    eyebrow: string;
    title: string;
    description: string;
    items: Feature[];
  };
  workflow: {
    eyebrow: string;
    title: string;
    description: string;
    before: string;
    beforeItems: string[];
    after: string;
    afterItems: string[];
    steps: Step[];
  };
  integrations: {
    eyebrow: string;
    title: string;
    description: string;
    cards: Array<{ label: string; title: string; description: string }>;
  };
  trust: {
    eyebrow: string;
    title: string;
    description: string;
    items: Array<{ title: string; description: string }>;
  };
  cta: { eyebrow: string; title: string; description: string; install: string; github: string };
  footer: { pitch: string; product: string; resources: string; readme: string; releases: string; architecture: string; copyright: string };
};

export const copy: Record<Locale, Copy> = {
  en: {
    meta: {
      title: 'sshx — SSH, without the distance',
      description: 'A transparent OpenSSH enhancement for remote-to-local commands, automatic port forwarding, workspace mounts, Docker, and editor integrations.',
    },
    nav: { features: 'Features', workflow: 'How it works', integrations: 'Integrations', github: 'GitHub', language: '简体中文' },
    hero: {
      badge: 'Open source · macOS + Linux',
      line1: 'SSH, without',
      line2: 'the distance.',
      description: 'Keep the OpenSSH workflow you already trust. Add a secure command bridge, automatic port forwarding, local domains, and bidirectional workspaces when you need them.',
      install: 'npm install -g @hahahhh/sshx',
      copy: 'Copy install command',
      copied: 'Copied',
      github: 'Explore on GitHub',
      note: 'Native binaries · Node.js 18+ wrapper · no account required',
      signals: ['OpenSSH compatible', 'No manual -L flags', 'Zero side effects when idle'],
      demoAlt: 'Animated sshx demo showing a remote development server opening in the local browser',
    },
    features: {
      eyebrow: 'ONE CONNECTION · BOTH SIDES',
      title: 'Your remote machine, with local superpowers.',
      description: 'sshx stays out of the way for ordinary SSH sessions and brings the two environments together when your workflow calls for it.',
      items: [
        { number: '01', title: 'Reach back to local', description: 'Run a command on your Mac or Linux client from inside the remote shell. Stdout, stderr, stdin, and exit codes cross the bridge intact.', command: 'sshx local pbcopy < build.log', span: 'wide' },
        { number: '02', title: 'Ports appear automatically', description: 'Remote listeners are detected and exposed through a stable local domain—without planning an SSH tunnel first.', command: 'myserver.alex.sshx:3000' },
        { number: '03', title: 'Workspaces cross the wire', description: 'Opt in to a bidirectional FUSE workspace so remote tools can edit local files and local tools can inspect remote work.', command: 'features.remoteFs: true' },
        { number: '04', title: 'Still real OpenSSH', description: 'Flags, config, jump hosts, authentication, and connection behavior continue through the OpenSSH you already use.', command: 'alias ssh=sshx', span: 'wide' },
      ],
    },
    workflow: {
      eyebrow: 'A SHORTER PATH',
      title: 'From remote process to local experience.',
      description: 'Connect normally. sshx creates the bridge around the session and removes the repetitive setup between your remote runtime and local desktop.',
      before: 'The old routine',
      beforeItems: ['Choose ports before connecting', 'Maintain -L flags per terminal', 'Copy files or commands by hand', 'Reconnect when the plan changes'],
      after: 'With sshx',
      afterItems: ['Connect with the same SSH host', 'Discover listeners as they start', 'Use a memorable local domain', 'Call local tools from the remote shell'],
      steps: [
        { label: 'CONNECT', title: 'Use your existing host', description: 'No new inventory or connection format. sshx delegates resolution and transport to OpenSSH.', command: 'sshx myserver' },
        { label: 'BUILD', title: 'Start the remote process', description: 'sshx notices loopback and wildcard listeners while your development session stays interactive.', command: 'npm run dev' },
        { label: 'OPEN', title: 'Finish on your local machine', description: 'Open the forwarded app—or invoke any allowed local command—without leaving the remote shell.', command: 'sshx local open http://myserver.alex.sshx:3000' },
      ],
    },
    integrations: {
      eyebrow: 'MEETS YOU WHERE YOU WORK',
      title: 'CLI-first. Editor-ready. Container-aware.',
      description: 'Use sshx directly in the terminal, with running Docker containers, or behind the Remote SSH workflow your editor already understands.',
      cards: [
        { label: 'FOUNDATION', title: 'OpenSSH', description: 'Pass-through compatibility for flags, config files, jump hosts, authentication agents, and normal remote commands.' },
        { label: 'TARGET', title: 'Docker', description: 'Connect to a running container by name or ID and keep the command bridge through docker exec.' },
        { label: 'EDITOR', title: 'VS Code + Cursor', description: 'Install paired SSH/SCP shims and preserve the editor’s native Remote SSH connection experience.' },
      ],
    },
    trust: {
      eyebrow: 'TRANSPARENT BY DESIGN',
      title: 'Powerful when active. Quiet when it is not.',
      description: 'A tool that bridges machines should be explicit about its boundaries. sshx favors familiar transport, connection-scoped services, and visible opt-ins.',
      items: [
        { title: 'Connection-scoped', description: 'Sidecars and forwards are leased to live clients and exit after the final session disappears.' },
        { title: 'Policy-controlled', description: 'The remote-to-local bridge has a configurable deny list for commands you never want exposed.' },
        { title: 'RemoteFS is opt-in', description: 'Workspace mounts stay disabled until you enable them for a target you trust.' },
        { title: 'Easy escape hatch', description: 'Use --no-wrap or SSHX_DISABLE=1 whenever you want exact SSH passthrough.' },
      ],
    },
    cta: { eyebrow: 'READY IN ONE COMMAND', title: 'Keep SSH. Lose the ceremony.', description: 'Install the wrapper, connect to an existing host, and let the remote and local sides work together.', install: 'Install sshx', github: 'View source' },
    footer: { pitch: 'Transparent OpenSSH enhancement for modern remote development.', product: 'Product', resources: 'Resources', readme: 'README', releases: 'Releases', architecture: 'Architecture', copyright: 'Open source software.' },
  },
  'zh-cn': {
    meta: {
      title: 'sshx — 远程，不再遥远',
      description: '透明增强 OpenSSH，提供远程到本地命令桥、自动端口转发、双向工作区、Docker 和编辑器集成。',
    },
    nav: { features: '核心能力', workflow: '工作方式', integrations: '集成', github: 'GitHub', language: 'English' },
    hero: {
      badge: '开源 · 支持 macOS 与 Linux',
      line1: '远程，',
      line2: '不再遥远。',
      description: '保留你熟悉且信任的 OpenSSH 工作流，在需要时获得安全命令桥、自动端口转发、本地域名与双向工作区。',
      install: 'npm install -g @hahahhh/sshx',
      copy: '复制安装命令',
      copied: '已复制',
      github: '在 GitHub 上查看',
      note: '原生二进制 · Node.js 18+ 封装 · 无需注册账号',
      signals: ['兼容 OpenSSH', '无需手写 -L 参数', '空闲时零副作用'],
      demoAlt: 'sshx 动画演示：在远程启动开发服务并自动在本地浏览器中打开',
    },
    features: {
      eyebrow: '一次连接 · 打通两端',
      title: '让远程机器拥有本地能力。',
      description: '普通 SSH 会话中，sshx 保持安静；需要跨越两台机器时，它把远程运行时与本地桌面连接起来。',
      items: [
        { number: '01', title: '从远程调用本地', description: '在远程 Shell 内执行 Mac 或 Linux 客户端命令，stdout、stderr、stdin 与退出码完整传递。', command: 'sshx local pbcopy < build.log', span: 'wide' },
        { number: '02', title: '端口自动出现', description: '自动发现远程监听端口，并通过稳定的本地域名访问，无需提前规划 SSH 隧道。', command: 'myserver.alex.sshx:3000' },
        { number: '03', title: '工作区跨越网络', description: '按需启用双向 FUSE 工作区，让远程工具编辑本地文件，本地工具也能访问远程内容。', command: 'features.remoteFs: true' },
        { number: '04', title: '依然是原生 OpenSSH', description: '参数、配置、跳板机、认证和连接行为全部交给你原本使用的 OpenSSH。', command: 'alias ssh=sshx', span: 'wide' },
      ],
    },
    workflow: {
      eyebrow: '更短的路径',
      title: '远程运行，本地体验。',
      description: '像往常一样连接。sshx 围绕当前会话建立桥接，省去远程运行时与本地桌面之间重复的手工配置。',
      before: '传统方式',
      beforeItems: ['连接前先决定端口', '每个终端维护一组 -L 参数', '手工复制文件或命令', '需求变化后重新连接'],
      after: '使用 sshx',
      afterItems: ['继续使用原有 SSH Host', '服务启动时自动发现端口', '通过易记的本地域名访问', '在远程直接调用本地工具'],
      steps: [
        { label: '连接', title: '继续使用原有 Host', description: '无需维护新资产或改变连接格式，解析和传输仍由 OpenSSH 完成。', command: 'sshx myserver' },
        { label: '运行', title: '启动远程开发服务', description: '开发会话保持交互时，sshx 自动发现回环与通配监听端口。', command: 'npm run dev' },
        { label: '打开', title: '在本地完成最后一步', description: '无需离开远程 Shell，即可打开转发后的应用或执行任意允许的本地命令。', command: 'sshx local open http://myserver.alex.sshx:3000' },
      ],
    },
    integrations: {
      eyebrow: '融入现有工作流',
      title: 'CLI 优先，编辑器与容器就绪。',
      description: '直接在终端使用 sshx，连接运行中的 Docker 容器，或接入编辑器已经理解的 Remote SSH 工作流。',
      cards: [
        { label: '基础', title: 'OpenSSH', description: '兼容参数、配置文件、跳板机、认证代理和普通远程命令。' },
        { label: '目标', title: 'Docker', description: '通过名称或 ID 连接运行中的容器，并在 docker exec 中继续使用命令桥。' },
        { label: '编辑器', title: 'VS Code + Cursor', description: '安装成对的 SSH/SCP shim，保留编辑器原生的 Remote SSH 连接体验。' },
      ],
    },
    trust: {
      eyebrow: '透明设计',
      title: '使用时强大，不使用时安静。',
      description: '跨机器桥接工具应该明确自己的边界。sshx 使用熟悉的传输方式、与连接绑定的服务和清晰的按需开关。',
      items: [
        { title: '随连接存在', description: 'Sidecar 和转发由活动客户端续租，最后一个会话消失后自动退出。' },
        { title: '策略控制', description: '远程到本地命令桥提供可配置的拒绝列表，阻止不应暴露的命令。' },
        { title: 'RemoteFS 按需启用', description: '只有你为可信目标显式开启后，工作区挂载才会生效。' },
        { title: '随时退出增强', description: '使用 --no-wrap 或 SSHX_DISABLE=1 即可精确透传到 SSH。' },
      ],
    },
    cta: { eyebrow: '一条命令即可开始', title: '保留 SSH，省去折腾。', description: '安装封装器、连接已有主机，让远程端和本地端自然协作。', install: '安装 sshx', github: '查看源码' },
    footer: { pitch: '面向现代远程开发的透明 OpenSSH 增强工具。', product: '产品', resources: '资源', readme: '使用文档', releases: '版本发布', architecture: '系统架构', copyright: '开源软件。' },
  },
};
