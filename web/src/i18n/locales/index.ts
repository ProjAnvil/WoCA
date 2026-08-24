// Locale message modules. One file per domain namespace; index.ts merges
// them into the two message trees passed to vue-i18n. New domains: add a
// module under zh-CN/ and en/, then import + spread it below.
import agentEn from './en/agent'
import adminEn from './en/admin'
import fsEn from './en/fs'
import loginEn from './en/login'
import statusEn from './en/status'
import apiEn from './en/api'
import chatEn from './en/chat'
import commonEn from './en/common'
import goalEn from './en/goal'
import memoryEn from './en/memory'
import memoryStoreEn from './en/memoryStore'
import nativeEn from './en/native'
import sessionsEn from './en/sessions'
import settingsEn from './en/settings'
import sidebarEn from './en/sidebar'
import workspaceEn from './en/workspace'
import agentZh from './zh-CN/agent'
import adminZh from './zh-CN/admin'
import fsZh from './zh-CN/fs'
import loginZh from './zh-CN/login'
import statusZh from './zh-CN/status'
import apiZh from './zh-CN/api'
import chatZh from './zh-CN/chat'
import commonZh from './zh-CN/common'
import goalZh from './zh-CN/goal'
import memoryZh from './zh-CN/memory'
import memoryStoreZh from './zh-CN/memoryStore'
import nativeZh from './zh-CN/native'
import sessionsZh from './zh-CN/sessions'
import settingsZh from './zh-CN/settings'
import sidebarZh from './zh-CN/sidebar'
import workspaceZh from './zh-CN/workspace'

export const zhCN = {
  common: commonZh,
  fs: fsZh,
  admin: adminZh,
  login: loginZh,
  status: statusZh,
  chat: chatZh,
  sidebar: sidebarZh,
  goal: goalZh,
  memory: memoryZh,
  settings: settingsZh,
  api: apiZh,
  agent: agentZh,
  sessions: sessionsZh,
  workspace: workspaceZh,
  memoryStore: memoryStoreZh,
  native: nativeZh,
}

export const en = {
  common: commonEn,
  fs: fsEn,
  admin: adminEn,
  login: loginEn,
  status: statusEn,
  chat: chatEn,
  sidebar: sidebarEn,
  goal: goalEn,
  memory: memoryEn,
  settings: settingsEn,
  api: apiEn,
  agent: agentEn,
  sessions: sessionsEn,
  workspace: workspaceEn,
  memoryStore: memoryStoreEn,
  native: nativeEn,
}
