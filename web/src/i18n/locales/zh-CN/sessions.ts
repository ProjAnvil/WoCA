export default {
  demo: {
    reply: '（演示模式）这是对「{prompt}」的示例回复，当前未连接后端。',
    codeBody: '这里会显示流式 token、工具调用与结果。',
    realMode: '- 真实模式通过 `POST /api/runs` + SSE 流式返回',
    toolCards: '- 工具调用与审批会以卡片/弹窗呈现',
  },
  defaultWorkspace: '默认工作区',
  newSession: '新会话',
  imageMessage: '图片消息',
  sessionBusyDeleteFirst: '会话正在运行，请先停止再删除',
  commands: {
    permissionSwitched: '权限模式已切换为 {label}（{mode}），后续工具调用立即生效。',
    permissionCurrent:
      '当前权限模式：{label}（{mode}）。\n用 /permissions <default {|} acceptEdits {|} bypassPermissions {|} readOnly {|} plan> 切换。',
    thinkingSet: '思考级别已设为 {label}（{budget} tokens），下一轮对话生效。',
    thinkingCurrent:
      '当前思考级别：{label}（{budget} tokens）。\n用 /thinking <off {|} low {|} medium {|} high> 切换。',
    failed: '⚠ 命令执行失败：{message}',
  },
}
