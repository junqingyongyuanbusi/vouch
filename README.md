# VOUCH — Validation Of Untrusted Code Hypotheses

> *husky for agent hooks — 但带差分证据。*

单二进制 `vouch`，本地 · 确定性 · 差分语义 · 零遥测。
在增量场景（受影响测试 + 基线缓存命中）下秒级拿到一份可复现的证据包；冷启动如实报告耗时。

> **首屏命令（P1）**：`vouch`（= verify）· `vouch init <agent>` · `vouch show [id]` · `vouch version`。
> 内部命令（可用但不进首屏）：`vouch rerun <id>` · `vouch gc` · `vouch mcp`（agent 回路）。
> 仍为 stub（exit 2）：`export`（P2）· `probe list` / `probe scaffold`（P3）。

## 核心原则

1. **证据与意见物理隔离** — `deterministic`（跑出来的）与 `inferred`（LLM推断）分数组、分渲染、分gate。`inferred` 永不参与 `VERIFIED/BROKEN` 判定，`--no-llm` 全可用。
2. **差分是原子操作** — 同一组探针跑 `base` 与 `candidate` 双 worktree，取 `delta`。天然吸收 `flaky` 与坏环境（双挂=无信号而非误报）。
3. **验不了就说验不了** — 只有 `VERIFIED(0) / BROKEN(1) / UNVERIFIED(2)` 三态，无分数。

## 安装

```bash
npx vouch verify                       # npm 入口（需 git ≥ 2.41；npm 包尚未发布，见下）
go install github.com/junqingyongyuanbusi/vouch/cmd/vouch@latest
# 或从源码
git clone https://github.com/junqingyongyuanbusi/vouch && cd vouch && make build
```

npm wrapper 按平台分发二进制（`@vouch/cli-<os>-<arch>`），本仓库 `make npm-roundtrip`
会做一次真实的 `npm pack → npm install → npx vouch` 往返验证。

> **npm 入口状态**：打包/安装路径已在 CI 与本地往返验证通过，但**尚未发布到 npm registry**。
> 发布前请使用下面的 `go install` 或源码构建；此时 `vouch init` 若在 PATH 找不到 vouch，
> 会写入 `npx --yes vouch …` 启动器并**明确提示**该配置依赖尚未发布的 npm 包。

## 快速开始

```bash
vouch verify                          # 校验当前工作区改动，退出码 = 结论
vouch verify --ci                     # CI 门禁：0=VERIFIED / 1=BROKEN / 2=UNVERIFIED
vouch verify --from main --to feat    # 分支区间
vouch verify --json --output out.json # 机器可读证据包
vouch show [--path repo] [bundle-id]  # 查看已落盘证据包（默认最新）
vouch init claude-code                # 一键接入 agent：注册 MCP server + 写入 Stop hook
vouch version

# 内部命令（同样可用，只是不在首屏）
vouch rerun <bundle-id> [--probe test] # 用记录的 base/candidate 重跑该探针（复现）
vouch gc --keep 50                     # 清理旧证据包与快照 ref
```

**“零配置”的口径**：指零 vouch 配置（不写 yaml、不登录）；**依赖按下方前置条件预先安装**。若希望 verify 自行安装 JS 依赖，显式传 `--install-deps`（会联网，默认关闭）。

**前置条件（P1，verify 默认不会替你安装）**

* JS/TS：仓库需已 `npm/pnpm/yarn install`；缺少 `node_modules` 时输出明确的 gap，或显式 `vouch verify --install-deps`（会联网，默认关闭）
* Python：需预装 pytest（`python3 -m pip install pytest`），否则记 gap 并给出 UNVERIFIED
* Go：无额外依赖（模块缓存由 go 自身管理）

**工作区副作用**：dirty 工作区的 `verify` 会用 `refs/vouch/snapshots/<sha>` 钉住候选快照，便于 `rerun` 复现；该命名空间由 verify 自动保留最近 20 个，`vouch gc` 可清理更早的 ref 与旧证据包。

## Agent 回路（P1.5，已可用）

```bash
vouch init claude-code
#   ✓ .mcp.json              → MCP server: agent 可随时调用 vouch_verify / vouch_show / vouch_gaps
#   ✓ .claude/settings.json  → Stop hook: agent 声称完成时自动 verify
#       BROKEN     → 阻塞（exit 2）并把证据回塞给 agent
#       VERIFIED   → 放行；UNVERIFIED → 只报告原因、不阻塞
vouch mcp                            # 手动运行 MCP server（stdio），通常由 agent 配置拉起
```

回路契约、工具清单、启动器（`PATH` / `npx`）与适配器范围见 `docs/agent-loop.md`。

## Roadmap（仍未实现）

```bash
vouch export --bundle a3f9e2         # [P2] 导出 PR markdown（show --md 已有最小版）
vouch probe scaffold <name>          # [P3] 探针脚手架
```

* [P1.5 未做] MCP registry 挂载（官方 / Smithery / PulseMCP）
* [P1.5 未做] 无剪辑 asciinema 回路演示
* [P1.5 延后] Cursor 适配器（计划要求先把 Claude Code 做到完美；契约已记录在 `docs/agent-loop.md`）

## 文档

* `docs/agent-loop.md` — agent 回路契约（MCP 工具、Stop hook 退出码映射、启动器、适配器范围）
* `docs/probe-protocol.md` — 探针协议 v1（对外稳定）
* `docs/bundle-schema.json` — 证据包 JSON Schema v1
* `docs/adr/` — 架构决策记录

## 探针

内置（P1）：`build` | `test` | `typecheck`（均走同一 `stdio JSON` 协议，崩溃隔离为 `inconclusive`）

第三方：可执行文件放 `~/.vouch/probes/` 或 `.vouch/probes/` 即被发现，`vouch probe scaffold` 一键生成模板。

## 验收与金丝雀

* 单元/集成：`go test ./... -count=1`（含 5 场景 × 3 生态矩阵、CLI 端到端、并发与身份稳定性）
* 检测率金丝雀：`make canary` → `internal/detector` 在 3 fixture + 5 真实仓上 ≥7/8
* 验证级金丝雀：`make canary-verify` → 真实仓跑 `vouch verify --ci`，输出
  三态成功率 / UNVERIFIED 率 / 增量 P50（CI 每晚 `canary.yml`）

最近一次本机结果（10 仓：Go×3 / Python×3 / JS×4，依赖按前置条件预装）：

```
three-state ok:   9/10      (target >= 7/10)
UNVERIFIED rate:  1/10      (target <= 20%)
incremental P50:  ~3.4s     (target <= 60s)
失败样本：sindresorhus/execa（测试入口是 npm run lint && unit && type，AVA 不在内置解析器内
→ 120s 探针预算耗尽 → UNVERIFIED，并明确记录 unverified_claims）
```

## 开发

```bash
make lint    # golangci-lint
make test    # go test ./...
make build   # bin/vouch
make canary  # 本地跑金丝雀（需网络）
```

`internal/` 依赖单向：`接口层(cobra/mcp/tui)` → `编排层(detector/selector/planner/scheduler)` → `执行层(worktree/sandbox/probe)` → `证据层(bundle/cache/differ/verdict)`。

## 零遥测

`100% local, zero telemetry` — 不上报任何运行数据。质量看金丝雀每晚跑的 10 个真实仓库，采纳看 `npm` 周下载斜率。

## License

Apache-2.0（含专利授权，见 `LICENSE`）

## 仓库与模块名说明

* 二进制名：`vouch`（`cmd/vouch`），`go.mod` 在仓根：`module github.com/junqingyongyuanbusi/vouch`
* 本地目录名 `acdc/` 为历史名（ACDC→VOUCH 更名），不影响 `go install github.com/junqingyongyuanbusi/vouch/cmd/vouch@latest`
* 远端确认后若为 `github.com/<org>/acdc`，需统一 `module` 与 README URL，本节同步更新

## 工具链

* `go 1.24+`，`golangci-lint v2`（CI 用 `golangci-lint-action@v6 latest`，本地按 https://golangci-lint.run/welcome/install/ 安装 v2）

