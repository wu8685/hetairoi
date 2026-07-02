# cma-service / ahsir / eventbus — 状态与 TODO

> 接续用(compact 后从这里恢复)。最后更新:2026-06-15。

## 1. 已完成并提交(均**本地提交,未 push**)

| 工作 | cma-service commit | ahsir commit |
|---|---|---|
| **P2** A2A 流式 / interrupt / 分页 / GC / 持久化 | `c7644bb` | `67ca6c6` |
| **A** 资源 CRUD 补全 + archive 生命周期 hardening | `538287e` `085fa3e` | — |
| **C1** 可观测性:agent.tool_use / mcp_tool_use / span.* | `983d49b` | `1053d40` |
| **C2** 可观测性:agent.thinking / tool_result / mcp_tool_result | `5798373` | `a081930` |
| **Event Bus** spec + 核心 package | `5e3f974` | — |
| **Event Bus** driver + webhook + 可跑 example | `5899b45` | — |

**分支**:
- cma-service:`eventbus`(7 提交,**superset**,含上面全部)· `p2-streaming-interrupt-hardening`(前 5 个的子集)· `main`(仅 initial)
- ahsir:`inline-registration-streaming-cancel`(3 提交)· `main`

测试基线:cma-service 58 Go test(race)+ e2e 16/16 全绿;ahsir wrapper 189(scheduler 有 1 个**预存失败** `TestInvocationLedgerReplayIgnoresBadJSONLLines`,与本次无关)。

全部经**真实 ahsir + DeepSeek** 验证(inline 注册、流式、interrupt、多轮、C1/C2 观测事件、eventbus 全链路告警处置)。

## 2. 待办(TODO)

### 高优先 / 卫生
- [ ] **push + 合并分支**到 git.internal.example.com(CodeHub,PR 制)。**沙箱连不上 git.internal.example.com(SSH/HTTPS 超时),需在 VPN 环境 push**:
  - `git push origin eventbus`(cma-service,含全部)
  - `cd ../ahsir && git push codehub inline-registration-streaming-cancel`(**注意 remote 名是 `codehub`;勿带上无关的 README.md / orchestrator/SKILL.md 改动**)
- [ ] ahsir 预存失败测试 `TestInvocationLedgerReplayIgnoresBadJSONLLines`(非本次,环境相关)

### D — custom tools(高阶,**eventbus 闭环的前置**)
- [ ] 方案已与the ahsir maintainer讨论定:**MCP 桥**——把 custom tool 注册成 cma-service 自托管的 MCP 工具,用 **MCP 调用的天然阻塞**当 turn 挂起点,翻译成 CMA 的 `requires_action` 协议。基本不碰 ahsir runtime。
- [ ] 协议说明已存:`brain-spark/knowledge/raw/摘录/2026-06-15-cma-custom-tool-use-协议.md`
- [ ] 范围:先 custom tool 核心(`agent.custom_tool_use` → `status_idle{requires_action,event_ids}` → `user.custom_tool_result` → resume);`user.tool_confirmation`(预置工具审批,走 claude permission hook)单列、更难。
- [ ] 建议先做**最小可行性 spike**(让 DeepSeek 真调一个 cma-service 托管的 MCP 工具,确认 MCP 调用 block 到客户端回结果)。

### Event Bus
- [ ] spec:`docs/EVENTBUS-SPEC.md`(§12 = 验收/TDD 清单)。三档 policy(Stateless/Keyed/Routed)+ 去重(per-handler 持久化 rotate)+ webhook + driver 全做完。
- [ ] **闭环 C(`emit_event` 工具)**:依赖 D(custom tool);Event 模型的 `Hop/CauseID` + hop 守卫已就位,D 落地后一接就亮。
- [ ] 扩 example:消息驱动助理(Keyed by thread)、多 agent 协同(emit_event)。

### 其余 CMA SDK gap(见下方完整盘点)
- [ ] **B 层整组资源**:`files` / `skills` / `vaults` / `sessions.resources`(+ 字段级:mcp auth、vault 注入、custom skill 内容、resource 挂载)
- [ ] **multiagent**:`agent.thread_context_compacted` + thread 语义(走 ahsir rooms)
- [ ] **观测 C3**:per-assistant-message `agent.message`(文本与工具交错,重访 buffering)
- [ ] 零散:`sessions.list` 的 `created_at_*` 时间范围过滤(纯 cma-side 小活)

## 3. CMA SDK 覆盖 gap(完整盘点,对照 anthropic 0.97.0)

核心 4 资源**已完整**:agents / environments / sessions / sessions.events(方法 20/20)。

**整组未实现**(辅助 4 资源,0/20):
- `files`(upload/download/list/metadata/delete)
- `skills`(自定义 skill 内容上传)
- `vaults`(密钥库,给 MCP/工具鉴权)
- `sessions.resources`(给 session 挂 file + 授权 token;与 files/vaults 一套)

**事件**:入站 2/4(缺 `user.tool_confirmation` / `user.custom_tool_result`=D);出站已支持 message/status_*/error/deleted/rescheduled + C1/C2 的 tool_use/mcp_tool_use/tool_result/mcp_tool_result/thinking/span.*;缺 `agent.thread_context_compacted`(multiagent)。

## 4. 关键坑 / 环境约定(务必记住)

- **`GO111MODULE=on` 必须带**:模块在 `GOPATH/src` 下、全局 `GO111MODULE=off`;GOPATH 模式不认 `go 1.23` 指令 → Go 1.22 method 路由模式失效 → **所有路由 404**。所有 `go build/test/run` 都要前缀 `GO111MODULE=on`。
- **公司代理**:`http_proxy=http://127.0.0.1:7897` 会拦截 localhost 请求返回 404。测本地服务用 `httpx trust_env=False`(e2e 同款)或 `export no_proxy=127.0.0.1,localhost`;`curl` 默认走代理。
- **这台 Mac SIGKILL 直接执行的新构建二进制**(rc=137/空 log)→ 用 `go run`,不要跑 `go build` 出来的二进制。`go build`/`go vet` 仅用于检查。
- **git.internal.example.com 沙箱不可达**(SSH:22 / HTTPS:443 超时)→ push 必须在 VPN 环境。
- ahsir 有**两个无关的未提交改动**(`README.md`、`plugin/skills/orchestrator/SKILL.md`)——是the ahsir maintainer自己的,**不要 commit/碰**。
- ahsir-side 改动**先与the ahsir maintainer确认**(规矩)。

## 5. 关键文件 / 入口

- 协议层:`docs/DESIGN.md`(SDK↔cma-service↔ahsir 两层协议)· `docs/ROADMAP.md`(状态/相位)· `docs/EVENTBUS-SPEC.md`
- ahsir 对接:`internal/ahsir/{client,card,a2a}.go`(A2A JSON-RPC + DataPart 观测事件)
- 事件总线:`internal/eventbus/`(package)· `internal/api/busdriver.go`(driver)· `cmd/eventbus-example/` + `example/eventbus/`(可跑 demo,`run.sh`)
- ahsir 侧:`internal/wrapper/{card,server,session_claude,executor,session_types}.go`(inline 注册、streaming cancel、DataPart surface)
