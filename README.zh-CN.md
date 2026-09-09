# sub2api-quota-scheduler

[English](README.md)

**优先消耗快要过期的 Claude 7 天额度。** 这是一个面向
[sub2api](https://github.com/Wei-Shaw/sub2api) 的旁路调度器：按"距离重置还有多久、还剩多少没用"给分组内的账号动态排序，并可以给某个账号保留一部分私用额度。它只使用 sub2api 的管理 API，不改 sub2api 本身、不碰数据库、不重启服务。

- 单个静态 Go 二进制，仅标准库，约 6 MB，运行时约 3 MB 内存、30 ms。
- 由 systemd timer 在 sub2api 旁边定时运行，宿主机无需安装任何东西。
- 默认 shadow 模式：先只记录"本来会改什么"，确认无误后再切 apply。
- 排序永远不会打断已有的粘性会话；只有你显式开启的硬保留和临期抢流才会。
- 停掉 timer 就回到 sub2api 的原始行为。

## 现状

新项目：自 2026-09-04 起以 apply 模式跑在一个五账号 Claude 分组上，难免有毛边，欢迎反馈。已在 sub2api **v0.2.0** 上测试。这是独立的社区项目，与 sub2api 维护者无关。

## 痛点

Claude 订阅（Max、Team 等）按固定时间重置的 **7 天窗口**计量，重置时没用完的额度直接作废。把多个订阅放在 sub2api 的一个分组里之后，真正重要的问题不是"更偏好哪个账号"，而是"哪个账号的剩余额度马上要作废"。

sub2api v0.2.0 自己回答不了这个问题：

| sub2api 的行为 | 后果 |
| --- | --- |
| 新会话按**静态优先级**、负载、LRU 选账号 | 7 天重置时间从不参与选择 |
| `prefer_soonest_reset` 只比较 **5 小时**窗口 | 周额度照样过期作废 |
| 它记录了每个账号的 `7d` / `7d_oi` 响应头 | 数据有了，但没有任何调度逻辑用它 |
| 阈值功能可以在超过某个百分比时停调 | 所有窗口、所有账号共用一个数字，无法做单账号保留 |
| 粘性会话（1 小时、使用即续期）高于优先级 | 这是对的：正在跑的会话不该跳账号丢缓存 |

上游对应的功能请求 [#5583](https://github.com/Wei-Shaw/sub2api/issues/5583) 仍处于开放状态，相关的还有 [#979](https://github.com/Wei-Shaw/sub2api/issues/979) 和 [#5681](https://github.com/Wei-Shaw/sub2api/issues/5681)。fork sub2api 意味着每个上游版本都要重新打补丁，而 sub2api 一周发好几个版本。所以本项目走旁路：只用 sub2api 已经采集的数据和已经暴露的管理 API。

两个具体需求驱动了设计：

1. **别浪费快过期的额度。** 一个明天重置、还剩 70% 的账号，应该比下周才重置的账号先接新会话，哪怕平时更偏好后者。
2. **保留私用额度。** 某个订阅同时也是自己聊天用的，网关流量不能把它推过设定的水位；对有独立子窗口的模型（Fable 系列用的 `7d_oi` 窗口）可以另设一个更高的水位。

## 工作原理

每轮（默认每 5 分钟）：

1. **读取**分组账号和分组模型路由。任一配置账号缺失或不在分组内，本轮直接放弃、不写任何东西。
2. **归一化**每个订阅账号的 7 天窗口（来自 sub2api 存在 `extra` 里的被动采样字段）：`known` 有用量和未来的重置时间；`idle` 重置时间已过且此后没有任何采样——Anthropic 的 7 天窗口以第一条消息为起点，所以此时新窗口根本还没开始，账号额度是满的、没有截止时间，留在基础顺序里，调度器绝不自己外推重置时间；`estimated` 是调度器自己的探测开启的窗口（见第 7 步），在拿到真实采样前按"探测时间 + 7 天、向上取整到小时"当作重置时间；`unknown` 字段缺失（sub2api 在每个新的 5 小时窗口会清空它们直到下次采样），回落基础顺序且不会触发保留动作。
3. **最后 5 小时最高优先级**：状态正常、可调度、7d 仍有可用额度且将在 5 小时内重置的订阅，直接排在所有其它账号之前，不受 `lookahead_hours`、最小余量或滞回限制。同档先用更早重置的账号，再比较剩余额度，最后按基础顺序。未启用 `enforce_ceiling` 的账号按 100% 计算容量，启用了保留的账号仍遵守自身上限。Fable 子窗口已满不会排除仍可供 Opus 等模型消耗的 7d 额度。**其后紧急池排序**：距重置不超过 `lookahead_hours`、且剩余额度不少于 `min_urgent_headroom_percent` 的订阅进入紧急池，按 `压力 = 剩余额度 / 距重置小时数` 排序；两个紧急账号只有在压力差超过 `hysteresis_ratio` 时才互换位置，避免抖动。
4. **写入优先级**：最后 5 小时档、紧急池、其余账号依次排列，其余按配置的基础顺序，写成 1..N。设置了 `window_max_age_hours` 的账号先做一次时效检查：7d 采样比它更旧、或者根本没有采样时间，就打一条 warning 并当作没有窗口来排。sub2api 自己采样的窗口不要设这个值——它们不会过期，因为只有账号服务请求时窗口才会变，而那次请求本身就会重新采样。只有窗口由调度器之外的东西写入时才需要设：写入方一停，账号退回基础顺序，而不是继续按一组已经不再变化的数字排序。只写线上值不同的账号，请求体只有 `priority`。
5. **执行保留**（标记了 `enforce_ceiling` 的账号）：7 天用量达到 `ceiling_percent` 就设为不可调度直到窗口重置再恢复，且只恢复自己关掉的账号；Fable（`7d_oi`）用量达到 `fable_ceiling_percent` 就写一条分组路由把 `fable_model_pattern` 导向其它账号，重置后撤销。不是自己写的路由一律不动，只告警。
6. **临期抢流**（可选，`drain_hours`）：某个未被排除的订阅在这么多小时内就要重置时，写一条分组路由（`drain_model_pattern`，默认 `claude-*`）只指向该账号。sub2api 在粘性之前先看路由，所以**已有会话**也会转到它上面；它被限流时 sub2api 回落到正常的优先级选择，恢复后路由再把流量拉回来。每次切换丢一次提示缓存，抢流期间 relay 被绕过。标记了 `enforce_ceiling` 或 `drain_exempt` 的账号永远不会成为抢流目标。开启一次抢流要求剩余额度不低于 `min_urgent_headroom_percent`，避免为了几个百分点把所有活跃会话都搬走；已经由调度器开启的抢流不受此限制，会一直持续到该账号触及自身上限（未保留账号即 100%）。
7. **重启闲置窗口**（可选，`restart_idle_windows`）：处于 `idle`、状态正常、可调度且未标记 `probe_exempt` 的订阅账号，会通过 sub2api 的账号测试接口收到一条探测消息（一句 "hi"，模型由 `probe_model` 指定，默认 `claude-haiku-4-5-20251001`）。这一条消息足以让 Anthropic 开启下一个 7 天窗口，账号一周后就能再次进入紧急池，而不是永远闲置在 relay 后面。测试接口不会回写用量采样，所以调度器把探测（连同当时看到的那个已结束窗口）记进状态文件，把窗口当作 `estimated`，直到 sub2api 采到任何更新的数据：无论是活跃窗口还是更晚结束的窗口，都会让估算作废。完全没有 7 天采样的账号也按同样方式探测。条件：账号状态正常、可调度或正被本轮解除保留、未标记 `probe_exempt`。两次探测至少间隔 `probe_cooldown_hours`；失败也会记录并在冷却后重试；每轮最多发 3 次探测，每次独立 60 秒超时。探测总是最后执行，不影响本轮排序。
8. **记录**一行 JSON 决策到 stdout 和 `decisions.jsonl`，并保存一个很小的状态文件。

除保留和抢流之外都是"软"的：只影响**新**会话落在哪里。已有会话由 sub2api 的粘性机制留在原账号，sub2api 自己的限流和阈值逻辑照常生效。

### Opus 子 agent 与粘性

更换模型不等于更换粘性会话。sub2api 从 `metadata.user_id` 提取 session ID，按分组和 session 绑定账号，不按模型或子 agent ID 隔离。Opus 子 agent 如果与 Fable 主 agent 使用同一个 session ID，原账号仍可用时粘性优先于 priority；独立的新 session 才会按优先级等规则重新选择。

现有抢流可以配置 `drain_hours: 5`、`drain_model_pattern: "claude-opus-*"`，将 Opus 导向临期账号，已有 Opus 会话也会受影响。模型路由候选中若仍包含原粘性账号，它仍可能优先被选中，所以仅把目标排在路由列表第一位并不能强制迁移。路由目标不可用时 sub2api 会回落。

要继续利用剩余 7d，sub2api 必须将 `7d_oi` 用尽作为 Fable 模型级限制处理；整账号停调或共享 5h/7d 用尽仍会阻止 Opus。模型路由可能更新共享的粘性绑定，因此只加 Opus 路由并不能保证主 agent 的下一次 Fable 请求留在原账号。应避免重叠的通配符路由：已检查的 sub2api 匹配器不保证优先选择更具体的通配符。

## 快速开始

1. 从 [Releases](https://github.com/reed-yang/sub2api-quota-scheduler/releases) 下载 `sub2api-quota-scheduler-linux-amd64` 或 `-arm64`，或本地 `sh build.sh` 编译。
2. 在 sub2api 后台生成 admin API key。
3. 复制 `deploy/config.example.json` 为 `config.json`，填 `group_id`、按偏好顺序列出账号 ID，需要保留的账号加上 `ceiling_percent` / `fable_ceiling_percent` / `enforce_ceiling`。`mode` 先保持 `shadow`。
4. 把二进制（命名为 `sub2api-quota-scheduler`）、`config.json`、`deploy/` 下两个 unit 文件和 `install.sh` 放到宿主机同一目录，然后：

   ```sh
   sudo sh install.sh
   sudo sh -c 'umask 077; echo "SUB2API_QUOTA_SCHEDULER_ADMIN_KEY=admin-..." > /etc/sub2api-quota-scheduler/env'
   sudo systemctl start sub2api-quota-scheduler.service
   journalctl -u sub2api-quota-scheduler -n 3 -o cat
   ```

5. 观察一段时间 shadow 判定：`sudo tail -n 1 /var/lib/sub2api-quota-scheduler/decisions.jsonl` 或 `journalctl -u sub2api-quota-scheduler -o cat`。
6. 把 `/etc/sub2api-quota-scheduler/config.json` 里的 `mode` 改为 `apply`。

回滚：`sudo systemctl disable --now sub2api-quota-scheduler.timer`。优先级、可调度开关、分组路由都是普通字段，随时可在后台手改。

## 配置项

见 [`deploy/config.example.json`](deploy/config.example.json)。各键含义与英文 README 的配置表一致：`base_url`、`admin_key_env`、`mode`、`group_id`、`lookahead_hours`、`min_urgent_headroom_percent`、`hysteresis_ratio`、`default_ceiling_percent`、`default_fable_ceiling_percent`、`fable_model_pattern`、`drain_hours`（默认 0 关闭）、`drain_model_pattern`、`restart_idle_windows`（默认关闭）、`probe_model`、`probe_cooldown_hours`（默认 6）、`accounts[]`（账号可加 `drain_exempt`、`probe_exempt`、`window_max_age_hours`）。`kind` 为 `relay` 的账号（看不到内部额度的 API key 中转）永远只按基础顺序排；由 relay-sync 喂窗口的中转账号是例外，它按 `subscription` 声明并设 `window_max_age_hours`。

## relay-sync

中转账号是指向另一个网关的 API key，sub2api 看不到它背后的订阅窗口，调度器只能按基础顺序排它。如果那个上游本身是一个你有管理权限的 sub2api，`relay-sync` 可以补上这份数据：

```sh
sub2api-quota-scheduler relay-sync --config /etc/sub2api-quota-scheduler/relay-sync.json
```

它登录上游 admin API，在能服务这把 key 的账号里挑 7 天剩余额度最多的那个，把窗口合并进本地中转账号的 `extra`，之后调度器就能用和直连订阅一样的压力口径排它。配置项见英文 README 与 [`deploy/relay-sync.example.json`](deploy/relay-sync.example.json)。

同步是单向且只增的：绝不写上游，任何一步失败就什么都不写，于是同步停了之后样本自然过期，调度器退回基础顺序。这正是 `window_max_age_hours` 的用途，也是 `relay-sync` 在目标账号没有按这套配置声明时直接拒绝运行的原因——写到一个调度器仍当作 `relay` 的账号上会被忽略，写到一个没有时效上限的账号上则会被永远信任。上游空闲、没有新样本可抄时会报一行并以 0 退出，定时器不会每刻钟失败一次。

上游密码是别人网关的真实凭据：放在 `/etc/sub2api-quota-scheduler/relay-sync.env`（0600），它只用来换 token，token 以 0600 缓存在 state 目录里复用到过期。

## 安全边界

- 读：`GET /api/v1/admin/accounts?group=<id>`、`GET /api/v1/admin/groups/<id>`。
- `relay-sync` 另外读本地 `GET /api/v1/admin/accounts/<id>`，以及上游的 `POST /api/v1/auth/login` 和 `GET /api/v1/admin/accounts`；唯一的写是 `POST /api/v1/admin/accounts/bulk-update`，只带一个账号 id 和三个 `passive_usage_*` 键（合并而非替换 `extra`），写完回读确认，没落上就失败。它不写上游，不跟随 HTTP 重定向（否则登录请求体会被重放到别的主机），报错只带状态码、不带可能回显凭据的响应体。
- 写（仅 apply）：`PUT /api/v1/admin/accounts/<id>` 只带 `priority`；`POST /api/v1/admin/accounts/<id>/schedulable`；`PUT /api/v1/admin/groups/<id>` 只带 `model_routing` 与 `model_routing_enabled`。绝不发送 `extra`、`credentials`、`group_ids`、`status`。
- 开启 `restart_idle_windows` 后：`POST /api/v1/admin/accounts/<id>/test` 只带 `model_id`，每个账号每个冷却期最多一次、每轮最多三次，且只针对上次采样显示已经结束的窗口（或完全没有采样的账号）。这会通过该账号真实发送一条消息（几百 token）。sub2api 侧的副作用：测试成功会清掉该账号的限流记录；上游返回 403 时 sub2api 会把账号状态置为 `error`，从所有调度中移除，调度器之后既不会再探测也不会恢复它。
- 任何读取失败或账号缺失都会在写入前中止。
- admin key 只从环境变量读取，不会出现在任何日志里。详见 [SECURITY.md](SECURITY.md)。

## 局限

- 每个账号只有一个优先级，排序只看账号级 7 天窗口；`7d_oi` 只用于保留。
- 粒度是 timer 间隔，这不是按请求级的调度器。
- 被动采样只在账号有请求时刷新；旧样本是安全的下界。sub2api 的"主动查询"只对 `oauth` 类型账号真正请求 Anthropic，对 `setup-token` 账号只返回本地估算，所以调度器不用它。
- 探测之后窗口是估算值（探测时间 + 7 天、向上取整到小时），直到该账号第一次真实响应；如果你的套餐窗口锚点不同，以采样到的重置时间为准。决策日志里没有截止时间的窗口 `hours_to_reset` 为 `null`，`headroom_percent` 为完整上限。
- 它只能引导存在的流量，没人发请求时额度照样过期。

## 参与

欢迎带决策日志的问题报告、其它 sub2api 版本的兼容性反馈，以及小而聚焦的 PR。见 [CONTRIBUTING.md](CONTRIBUTING.md)。

## 许可

[MIT](LICENSE)。本项目不包含也不链接任何 sub2api 代码，只通过 HTTP 管理 API 与之交互。
