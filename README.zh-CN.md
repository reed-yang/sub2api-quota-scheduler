# sub2api-quota-scheduler

[English](README.md)

**优先消耗快要过期的 Claude 7 天额度。** 这是一个面向
[sub2api](https://github.com/Wei-Shaw/sub2api) 的旁路调度器：按"距离重置还有多久、还剩多少没用"给分组内的账号动态排序，并可以给某个账号保留一部分私用额度。它只使用 sub2api 的管理 API，不改 sub2api 本身、不碰数据库、不重启服务。

- 单个静态 Go 二进制，仅标准库，约 6 MB，运行时约 3 MB 内存、30 ms。
- 由 systemd timer 在 sub2api 旁边定时运行，宿主机无需安装任何东西。
- 默认 shadow 模式：先只记录"本来会改什么"，确认无误后再切 apply。
- 排序永远不会打断已有的粘性会话；只有你显式配置的硬保留才会。
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
2. **归一化**每个订阅账号的 7 天窗口（来自 sub2api 存在 `extra` 里的被动采样字段）：`known` 有用量和未来的重置时间；`rolled` 重置时间已过但还没新采样，按用量 0、重置时间整周外推处理；`unknown` 字段缺失（sub2api 在每个新的 5 小时窗口会清空它们直到下次采样），回落基础顺序且不会触发保留动作。
3. **紧急池排序**：距重置不超过 `lookahead_hours`、且剩余额度不少于 `min_urgent_headroom_percent` 的订阅进入紧急池，按 `压力 = 剩余额度 / 距重置小时数` 排序；两个紧急账号只有在压力差超过 `hysteresis_ratio` 时才互换位置，避免抖动。
4. **写入优先级**：紧急池在前，其余按配置的基础顺序，写成 1..N。只写线上值不同的账号，请求体只有 `priority`。
5. **执行保留**（标记了 `enforce_ceiling` 的账号）：7 天用量达到 `ceiling_percent` 就设为不可调度直到窗口重置再恢复，且只恢复自己关掉的账号；Fable（`7d_oi`）用量达到 `fable_ceiling_percent` 就写一条分组路由把 `fable_model_pattern` 导向其它账号，重置后撤销。不是自己写的路由一律不动，只告警。
6. **记录**一行 JSON 决策到 stdout 和 `decisions.jsonl`，并保存一个很小的状态文件。

除保留之外都是"软"的：只影响**新**会话落在哪里。已有会话由 sub2api 的粘性机制留在原账号，sub2api 自己的限流和阈值逻辑照常生效。

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

见 [`deploy/config.example.json`](deploy/config.example.json)。各键含义与英文 README 的配置表一致：`base_url`、`admin_key_env`、`mode`、`group_id`、`lookahead_hours`、`min_urgent_headroom_percent`、`hysteresis_ratio`、`default_ceiling_percent`、`default_fable_ceiling_percent`、`fable_model_pattern`、`accounts[]`。`kind` 为 `relay` 的账号（看不到内部额度的 API key 中转）永远只按基础顺序排。

## 安全边界

- 读：`GET /api/v1/admin/accounts?group=<id>`、`GET /api/v1/admin/groups/<id>`。
- 写（仅 apply）：`PUT /api/v1/admin/accounts/<id>` 只带 `priority`；`POST /api/v1/admin/accounts/<id>/schedulable`；`PUT /api/v1/admin/groups/<id>` 只带 `model_routing` 与 `model_routing_enabled`。绝不发送 `extra`、`credentials`、`group_ids`、`status`。
- 任何读取失败或账号缺失都会在写入前中止。
- admin key 只从环境变量读取，不会出现在任何日志里。详见 [SECURITY.md](SECURITY.md)。

## 局限

- 每个账号只有一个优先级，排序只看账号级 7 天窗口；`7d_oi` 只用于保留。
- 粒度是 timer 间隔，这不是按请求级的调度器。
- 被动采样只在账号有请求时刷新；旧样本是安全的下界。
- 它只能引导存在的流量，没人发请求时额度照样过期。

## 参与

欢迎带决策日志的问题报告、其它 sub2api 版本的兼容性反馈，以及小而聚焦的 PR。见 [CONTRIBUTING.md](CONTRIBUTING.md)。

## 许可

[MIT](LICENSE)。本项目不包含也不链接任何 sub2api 代码，只通过 HTTP 管理 API 与之交互。
