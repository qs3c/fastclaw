# 记忆提取触发机制 — 方案讨论记录

> 本文件由 Claude Code 会话导出,记录了关于 `internal/agent/loop.go` 异步记忆提取触发机制重构的完整讨论。

---

## 问题 1（用户）

请详细分析目前turn结束时触发的异步记忆提取方案设计（在internal/agent/loop.go），现在的逻辑是根据user message 数量，当其是5的倍数时触发，这是非常不合理的。因为目前系统中turn是可以并发执行的，可能导致重复触发或者不触发的问题。我需要你拟定一个新的方案，要求是每5个完整执行完的turn触发一次提取，这些turn可以是跨session的，来自不同session的5个turn。例如，第五个turn执行完毕后，如果前面的turn还在运行没有完成，也不可以进行提取。以及还要考虑到上下文压缩的影响，虽然现在的上下文压缩中还没有实现压缩前触发一次额外的记忆提取，但是后续我会做这件事，所以你要把这一点考虑进去，但是压缩带来的额外提取是不跨session的，只把当前session还没被提取过的turn拿去提取。所以这综合起来可能会有点复杂，你可以引入新的数据库表，引入checkpoint机制之类的，你自己评估。只需给出方案，无需做具体实现。

### 回答

我先来分析现有代码,了解当前的触发逻辑和相关数据结构。

---

### 回答

我先定位现有的触发逻辑和相关持久化代码。

---

### 回答

我已经把现有触发逻辑、`session_messages` 表、`AutoPersistMemory`、压缩逻辑(`CompactMessages`)的调用位置都看完了。下面是现状分析和新方案设计。

---

# 异步记忆提取触发机制重构方案

## 一、现状分析(问题确认)

现有逻辑(`internal/agent/loop.go` 的 `runPostTurn`,约 2437–2482 行):

1. 每个 turn 结束时调用 `dataStore.CountChatterUserMessages(agent_id, chatter_user_id)`,统计该 chatter 在该 agent 下**所有 session** 的 `session_messages` 中 `role='user'` 的总行数 `n`;
2. 当 `n % EveryNTurns == 0`(默认 5)时,`go AutoPersistMemory(...)` 异步提取;
3. 提取的输入是**当前触发 turn 所在 session 的内存工作集的最后 20 条消息**。

在 turn 可并发(同一 agent + chatter 的不同 session 同时跑)的前提下,问题确实成立,且不止你说的两点:

| # | 问题 | 成因 |
|---|------|------|
| 1 | **重复触发** | user message 行在 turn **开始**时就插入归档表,turn **结束**时才查数。两个并发 turn 都先插入,各自结束时都读到 `n=10`,模 5 都命中,提取被触发两次,且输入高度重叠 |
| 2 | **漏触发** | 并发插入使某个 turn 观测到的计数从 4 直接跳到 6,5 这个倍数点没有任何 turn 在结束时刚好观测到,本轮提取永久丢失 |
| 3 | **计数≠turn** | steering 注入(`PushSteerIfActive`)、群聊广播(`InjectGroupMessage`)都会追加 user 行,计数和"完整 turn 数"早已脱钩 |
| 4 | **提取输入与计数口径不一致** | 计数是跨 session 的,但提取输入只有当前 session 的最后 20 条内存消息——其他 session 贡献了计数,内容却从未被提取;同一 session 连续两次触发时窗口重叠,同样的内容被重复提取 |
| 5 | **受压缩干扰** | 输入取自内存工作集,压缩后工作集开头是 `[Conversation Summary]` 合成消息,提取的是摘要而非原文 |

## 二、设计目标

1. 触发单位是**完整执行完毕的 turn**(`runPostTurn` 到达 = turn 完成),每累计 5 个未提取的完成 turn 触发一次,可跨 session(范围限定在同一 `(agent_id, chatter_user_id)`,因为 MEMORY.md/USER.md 是 chatter 维度的);
2. **顺序约束**:只有当某个 turn 之前(按全局序)的所有 turn 都已终结时,它才有资格进入提取批次——第 5 个 turn 完成但第 3 个还在跑,不触发;
3. 并发安全:同一批 turn **恰好被认领一次**(不重复、不丢失);
4. 为未来"压缩前的 session 内提取"预留接口:压缩提取只取**当前 session 内尚未被任何提取消费过的已完成 turn**,且消费记录要让全局节拍提取能感知,避免同一 turn 被提取两遍;
5. 提取输入改为从 `session_messages` 归档表按 turn 精确读取,与内存工作集、压缩状态解耦;
6. 进程崩溃/重启后状态可恢复,不会永久卡死。

## 三、核心设计

### 3.1 新表一:`agent_turns`(turn 台账)

把"turn"提升为一等持久化实体,生命周期与 `HandleMessage`/`HandleMessageStream` 对齐:

```sql
CREATE TABLE agent_turns (
    user_id          TEXT NOT NULL,           -- agent owner,与 session_messages 同口径
    agent_id         TEXT NOT NULL,
    chatter_user_id  TEXT NOT NULL,
    turn_seq         INTEGER NOT NULL,        -- 按 (agent_id, chatter_user_id) 单调递增,turn 开始时分配
    session_key      TEXT NOT NULL,
    start_seq        INTEGER NOT NULL,        -- 本 turn 的 user message 在 session_messages 中的 seq
    end_seq          INTEGER,                 -- 完成时回填:本 turn 最后一条消息的 seq;运行中为 NULL
    status           TEXT NOT NULL DEFAULT 'running',  -- running | completed | failed | abandoned
    extraction_id    TEXT,                    -- NULL = 尚未被任何提取消费;非 NULL = 已被某次提取认领
    started_at       TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    finished_at      TIMESTAMP,
    PRIMARY KEY (agent_id, chatter_user_id, turn_seq)
);
CREATE INDEX idx_agent_turns_pending ON agent_turns
    (agent_id, chatter_user_id, status) WHERE extraction_id IS NULL;
CREATE INDEX idx_agent_turns_session ON agent_turns (agent_id, session_key, turn_seq);
```

**生命周期挂接点:**

- **turn 开始**(`HandleMessage` / `HandleMessageStream` 中 `sess.Append(userMsg)` 之后):插入一行,`turn_seq` 用与 `session_messages.seq` 相同的 `COALESCE(MAX(turn_seq),-1)+1` 事务内分配模式;`start_seq` 取刚归档的 user message 的 seq。slash command 等不进 ReAct 循环的路径**不**登记。
- **turn 结束**(`runPostTurn` 内):`UPDATE ... SET status='completed', end_seq=<该session当前最大seq>, finished_at=now()`。
- **异常路径**:用 `defer` 保证 provider 报错、panic 等出口把状态置为 `failed`(终结态,推进 frontier 但不进入提取输入)。

注意三点收益:`turn_seq` 在 turn 开始时分配,给出了一个**稳定的全局顺序**(这正是"前面的 turn"的定义);steering / 群聊注入消息天然落在所属 turn 的 `[start_seq, end_seq]` 区间内,不再污染计数;`(start_seq, end_seq)` 让提取可以从归档表精确回放一个 turn 的原文。

### 3.2 新表二:`memory_extractions`(提取批次 / checkpoint)

每次提取(无论全局节拍还是压缩触发)是一条持久化批次记录,兼任 checkpoint 和重试账本:

```sql
CREATE TABLE memory_extractions (
    id               TEXT PRIMARY KEY,        -- uuid
    agent_id         TEXT NOT NULL,
    chatter_user_id  TEXT NOT NULL,
    trigger          TEXT NOT NULL,           -- 'cadence' | 'compaction'
    session_key      TEXT NOT NULL DEFAULT '',-- compaction 触发时记录目标 session
    turn_count       INTEGER NOT NULL,
    status           TEXT NOT NULL DEFAULT 'claimed',  -- claimed | done | failed
    error            TEXT NOT NULL DEFAULT '',
    created_at       TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    finished_at      TIMESTAMP
);
```

`agent_turns.extraction_id` 指向这里。哪些 turn 属于哪个批次由外键反查,不需要再存范围字段。

### 3.3 完成边界(frontier)与触发条件

定义 **frontier F** = 满足"`turn_seq ≤ F` 的所有 turn 都处于终结态(completed/failed/abandoned)"的最大 `turn_seq`。计算方式:

```sql
F = COALESCE( (SELECT MIN(turn_seq) - 1 FROM agent_turns
               WHERE agent_id=? AND chatter_user_id=? AND status='running'),
              (SELECT COALESCE(MAX(turn_seq), 0) FROM agent_turns
               WHERE agent_id=? AND chatter_user_id=?) )
```

**触发条件**:`COUNT(turn WHERE status='completed' AND extraction_id IS NULL AND turn_seq <= F) >= N`(N = `memoryCfg.AutoPersist.EveryNTurns`)。

这个条件在**每个 turn 完成时**评估一次即可覆盖所有时序:你举的例子里,turn 5 先完成时 turn 3 还在跑,F < 5,不触发;turn 3 最后完成时,它自己的 `runPostTurn` 会再评估,此时 F=5、未提取完成数=5,触发。**frontier 的每次推进必然伴随某个 turn 的完成事件**,所以不需要轮询,也不会漏。

> 用"未提取完成数 ≥ N"而不是"checkpoint + N ≤ F"的标量水位线,是刻意的:压缩触发的提取会在序列中间"打洞"(把某个 session 的 turn 提前消费掉),标量水位线无法表达带洞的已提取集合,逐 turn 的 `extraction_id` 标记可以。副作用是压缩提取消费掉的 turn 不再计入全局节拍——这是合理的,否则全局提取会拿到一个残缺批次,重复消化已入库的内容。

### 3.4 原子认领(防重复触发的关键)

触发评估和认领放在**一个写事务**里(SQLite 用 `BEGIN IMMEDIATE`,Postgres 用 `SELECT ... FOR UPDATE`),伪代码:

```
BEGIN IMMEDIATE;
  F := 计算 frontier
  turns := SELECT * FROM agent_turns
           WHERE agent_id=? AND chatter_user_id=?
             AND status='completed' AND extraction_id IS NULL AND turn_seq <= F
           ORDER BY turn_seq LIMIT <batchCap>;   -- batchCap 建议 3N,见 3.7
  IF len(turns) < N { ROLLBACK; return }
  INSERT INTO memory_extractions (id, trigger='cadence', ...);
  UPDATE agent_turns SET extraction_id=:id WHERE <这批 turn 的主键>;
COMMIT;
→ 事务成功后才 go runExtraction(id, turns)
```

两个并发的 `runPostTurn` 同时到达:第二个事务执行时这批 turn 的 `extraction_id` 已非 NULL,查出来不足 N 条,直接放弃。**重复触发在事务层面被根除**,不依赖任何内存状态(也就天然免疫重启、UserSpace 失效、多副本部署)。

**"先标记后提取"的取舍**:认领成功即标记,LLM 调用失败不回滚标记,而是把 `memory_extractions.status` 置 `failed`。理由:输入持久在 DB 里,失败批次可以由后台或下次触发时**按批次重试**(重跑 `failed` 的 extraction 行);反之"先提取后标记"在崩溃时会把同一批内容二次写入 MEMORY.md。记忆提取丢一批是良性的,写重是脏数据。

### 3.5 提取输入:从归档表按 turn 回放

`AutoPersistMemory` 改为接受 turn 列表而非内存 `messages`:

1. 对批次内每个 turn,按 `(session_key, seq BETWEEN start_seq AND end_seq)` 从 `session_messages` 读原文;
2. 过滤 `origin != ''`(goal_context 等合成行)、过滤/截断 `tool` 角色行(沿用现在每条 300 字符的截断);
3. 按 session 分组拼 prompt(`### Session <title/key>` 分节),跨 session 批次对 LLM 仍然可读;
4. 现有的 `LoadMemory`/`SaveMemoryWithScan` 增量追加逻辑不变。

这同时修掉了问题 4 和 5:输入恰好等于被计数的那 5 个 turn,无重叠、无遗漏,且完全不受内存工作集压缩影响。

### 3.6 压缩触发的 session 内提取(预留路径)

未来在 `CompactMessages` 真正要压缩之前(loop.go 1851 / 2554 两个调用点),插入:

```
BEGIN IMMEDIATE;
  turns := SELECT ... WHERE agent_id=? AND session_key=?
             AND status='completed' AND extraction_id IS NULL
           ORDER BY turn_seq;          -- 注意:无 frontier 约束、无 ≥N 门槛
  IF len(turns) == 0 { ROLLBACK; skip }
  INSERT INTO memory_extractions (trigger='compaction', session_key=?, ...);
  UPDATE agent_turns SET extraction_id=:id ...;
COMMIT;
→ 异步执行提取(不阻塞压缩本身)
```

要点:

- **不跨 session、不看 frontier**:session 内的 turn 是串行的(一个 session 同时只有一个 in-flight turn,steering 走注入而非新 turn),已完成集合天然连续;其他 session 是否在跑无关紧要。
- **不设 ≥N 门槛**:压缩即将销毁原文细节,哪怕只有 1 个未提取 turn 也值得抢救。
- **与全局节拍自动协调**:被压缩提取消费的 turn `extraction_id` 已置位,全局节拍自然跳过它们,零额外协调逻辑。这就是 3.3 选择逐 turn 标记而非标量 checkpoint 的原因。
- 群聊 session 一个 session 可能有多个 chatter:压缩提取需按 `chatter_user_id` 分组,每组一个批次、写各自的 MEMORY.md/USER.md(turn 台账里已带 chatter 字段,分组是免费的)。

### 3.7 故障与边界处理

- **僵尸 turn 卡死 frontier**:进程崩溃会留下永久 `running` 的行,frontier 永不推进。两层兜底:① 触发评估时顺手把 `started_at` 超过阈值(建议 30 分钟,大于最长合理 turn)的 `running` 行置为 `abandoned`(终结态,推进 frontier,不进提取);② daemon 启动时做一次同样的清扫。
- **积压追赶**:如果因故(DB 抖动、提取持续失败)未提取完成数涨过 N,下次触发一次性认领,但用 `batchCap`(建议 3N)封顶防止 prompt 爆炸;超出部分留给再下一次完成事件。
- **失败重试**:`memory_extractions.status='failed'` 的批次,在该 chatter 下次任何 turn 完成时顺带重试一次(读 extraction_id 反查 turn 集合即可重建输入);重试仍失败就保留 failed 记录供排查,不无限重试。
- **降级**:`dataStore == nil`(无持久化的单机模式)整个机制关闭,与现状一致;`agent_turns` 写失败只记日志,绝不阻塞聊天主路径。
- **遗留行**:老的 `CountChatterUserMessages` 及其 store 接口方法可删除;`HookContext.TurnCount`(内存计数)与本机制无关,保留。

## 四、时序示例

N=5,session A/B 并发,数字为 `turn_seq`:

```
t1: turn1(A) 开始        running:{1}
t2: turn2(B) 开始        running:{1,2}
t3: turn2 完成           F=0(turn1还在跑) → 未提取完成数(≤F)=0,不触发 ✓
t4: turn1 完成           F=2 → 数=2,不触发
t5: turn3..5 依次开始/完成
t6: turn5 完成           F=5 → 数=5 ≥ 5 → 事务认领 turn1..5,异步提取 ✓
t7: (并发的另一个完成事件同时评估) → turn1..5 已被认领,数=0,静默跳过 ✓
...
t8: session A 触发压缩,A 中未提取的 turn7、turn9 被 compaction 批次认领
t9: turn10 完成,F=10 → 未提取完成数 = {6,8,10} = 3 < 5,不触发;
    再过两个 turn 凑满 5 个才触发,turn7/9 不会被重复提取 ✓
```

## 五、落地改动清单(供实现时参考)

1. `internal/store/database.go`:建表 + 迁移;新增 `BeginTurn` / `FinishTurn`(completed/failed)/ `SweepStaleTurns` / `ClaimCadenceBatch` / `ClaimCompactionBatch` / `FinishExtraction` 等 store 方法;删除 `CountChatterUserMessages`。
2. `internal/agent/loop.go`:`HandleMessage` / `HandleMessageStream` 在 `sess.Append(userMsg)` 后登记 turn(defer 收尾);`runPostTurn` 中现有的计数门改为"标记完成 → 事务认领 → 异步提取"。
3. `internal/agent/memory.go`:`AutoPersistMemory` 改为按 turn 集合从归档回放输入;增加按批次重试入口。
4. `internal/agent/compaction.go`:暂不改;未来压缩前提取在两个 `CompactMessages` 调用点前插入 `ClaimCompactionBatch` 即可。

## 六、被否决的备选

- **标量 checkpoint(只存 last_extracted_turn_seq)+ CAS 更新**:实现最简单,防重也够,但表达不了压缩提取打出的"洞",未来要么重复提取要么强行把洞并入批次,与需求 4 冲突,放弃。
- **内存级互斥(per-chatter mutex/sync.Once)**:解决不了重启丢状态和多副本部署("sibling replica" 在代码注释中已是明确场景),放弃。
- **后台轮询器评估触发**:不需要——frontier 只在 turn 完成时变化,事件驱动评估已完备,轮询徒增一个常驻组件。

方案的核心一句话:**把 turn 变成带状态机的持久化实体,frontier 由"最小 running turn_seq"定义,触发与认领在单个 DB 事务内完成,逐 turn 的 `extraction_id` 标记同时充当全局节拍和未来压缩提取的去重账本。**

---

## 问题 2（用户）

我在想是否有更简单的，解决这个问题的方式，比如我们在session_messages 表里增加两个字段。isTurnDone 标记一个Turn是否结束，因为在不考虑Steer消息的时候，Turn和User message其实是对等的概念。isExtracted 标记这个user message 是否被提取过了。这样只需要查表，过滤并计算出Turn已经结束的并且未被提取过的User message，如果达到5条就触发提取就好了吧，不用引入新表。如果要考虑上Steer的User message，那么就再加一个isSteer来区分普通的能代表一个Turn的User message和Steer 的User message。

### 回答

你的直觉是对的:在"turn ≈ 非 steer 的 user message"这个等价关系成立的前提下,user message 行本身就可以充当 turn 台账,`agent_turns` 表确实可以省掉。这个方案我认可,但照原样用三个布尔字段会踩到几个坑,需要做一些修正。逐条分析:

## 1. `isTurnDone` 不能是布尔,必须是状态枚举

布尔只能表达 running/done 两态,而崩溃恢复需要第三态。daemon 在 turn 中途崩溃后,这行 user message 永远停在 `isTurnDone=false`:

- 如果 frontier 逻辑存在(见第 3 点),它会**永久阻塞**后续所有提取;
- 如果直接把它扫成 `true`,它会作为"完成的 turn"进入提取批次,但它的 assistant 回复是残缺的。

所以这个字段应该是 `turn_status TEXT`:`'running' | 'done' | 'failed' | 'abandoned'`,配一个惰性清扫(触发评估时把 `running` 超过 30 分钟的置为 `abandoned`)。`failed/abandoned` 是终结态——推进 frontier,但不进提取输入。还是一个字段,只是从布尔换成枚举。

## 2. `isSteer` 可以不要,而且不要更安全

用**默认值语义**代替显式标记:`turn_status` 默认 `''`(空 = 不是 turn 锚点),只有 `HandleMessage`/`HandleMessageStream` 的 turn 开始路径(`sess.Append(userMsg)` 处)显式写入 `'running'`。这样:

- steer 注入(`PushSteerIfActive`)走的是另一条路径,自然落为 `''`;
- 更重要的是,它顺带覆盖了你方案里漏掉的**第三、四类 user 行**:`InjectGroupMessage` 注入的其他 bot 的群聊消息、`goal_context` 的合成续跑消息——它们都是 user role 但都不是 turn。靠 `isSteer` 一个布尔是兜不住这些的,白名单(显式标 running)比黑名单(标出每种非 turn)健壮得多;
- 历史存量行默认 `''`,自动不参与计数,不需要数据回填。

## 3. frontier(顺序约束)是这个方案里最别扭的一点

你描述的触发条件"已完成且未提取的 user message 达到 5 条"只覆盖了一半需求。考虑:turn 1 还在跑,turn 2–6 都完成了——计数=5,会触发,提取 2–6,把 1 跳过去了。这违反你自己定的"前面的 turn 没完成不可以提取"。所以仍然需要 frontier:**只统计/认领"开始时间早于最早 running 锚点"的行**。

问题在于 `session_messages` 的 `seq` 是 per-session 的,**没有跨 session 的全局序**。frontier 只能用 `created_at` 兜:

```sql
frontier_ts = COALESCE(
    (SELECT MIN(created_at) FROM session_messages
      WHERE agent_id=? AND chatter_user_id=? AND turn_status='running'),
    'infinity')
```

并发开始的两个 turn 时间戳可能同刻平局,顺序有毫秒级模糊。对记忆提取这个场景,批次边界差一个 turn 无伤大雅,我认为可以接受——但这是和独立 `turn_seq` 相比真实让渡掉的东西,值得知情。

## 4. `isExtracted` 建议升级为 `extraction_id`(或至少 `extracted_at`)

布尔能用,但提取是异步 LLM 调用,会失败。失败后两个选择:

- **补偿回滚**:把这批行的标记重置回未提取,下次触发自动重新覆盖。用布尔就够,但要求失败路径可靠执行(claim 后、写 MEMORY.md 前崩溃 → 这批永久丢失,只能认了,记日志);
- **批次重试**:那就需要知道"哪些行属于同一个失败批次",布尔表达不了,至少要 `extraction_id TEXT`。

我建议直接用 `extraction_id`(NULL = 未提取):成本和布尔一样,留了审计和重试的口子。但**批次状态本身**(failed/done/error 信息)没地方放——这是省掉 `memory_extractions` 表的真实代价。如果你接受"提取失败 = 补偿重置或丢一批,只记日志",就不需要那张表;以后想要可靠重试,再补表即可,列设计不用动。

## 5. 防重复触发的事务,一行都不能少

换表不豁免并发问题。"数够 5 条 → 置位认领"必须在一个写事务里(SQLite `BEGIN IMMEDIATE` / Postgres `SELECT ... FOR UPDATE`):

```
BEGIN IMMEDIATE;
  rows := SELECT ... WHERE agent_id=? AND chatter_user_id=?
            AND turn_status='done' AND extraction_id IS NULL
            AND created_at < frontier_ts
          ORDER BY created_at LIMIT 15;        -- 3N 封顶,防积压爆 prompt
  IF count < 5 → ROLLBACK
  UPDATE ... SET extraction_id=:uuid WHERE <这些行>;
COMMIT;  → 成功后才 go 提取
```

两个并发 `runPostTurn` 同时数到 5,第二个事务里行已被认领,数不够,静默退出。这是防重的全部,跟用哪张表无关。

## 6. 两个实现细节别漏

- **回填 done 需要定位锚点行**:turn 结束时要 UPDATE 自己那行,所以 `StoreAdapter.AppendMessage` 需要把分配到的 `seq` 返回出来(现在不返回),turn 期间在内存里持有 `(session_key, seq)`。
- **turn 的消息范围是推导出来的**:没有 `end_seq` 字段,提取时一个 turn 的消息 = 同 session 内 `[锚点.seq, 下一个锚点.seq)`(无下一个锚点则到当前最大 seq)。session 内 turn 串行,这个推导是可靠的,steer 消息自然落在所属区间里。压缩触发的 session 内提取也用同样的推导,逻辑完全复用。

## 结论

修正后的方案只在 `session_messages` 上加两列:

```sql
ALTER TABLE session_messages ADD COLUMN turn_status   TEXT NOT NULL DEFAULT '';
ALTER TABLE session_messages ADD COLUMN extraction_id TEXT;  -- NULL = 未提取
-- 部分索引:WHERE turn_status='done' AND extraction_id IS NULL
```

相比双表方案,你失去的是:全局 turn 序(用 created_at 近似)、批次级失败重试账本、`session_messages` "append-only 归档"的纯粹性(锚点行会被 UPDATE 两次)。换来的是少两张表、少一套生命周期代码。对这个功能的容错要求(丢一批记忆提取是良性的)来说,**这笔交换是划算的,我推荐采用你的方向 + 上述修正**:`isTurnDone` → `turn_status` 枚举、`isSteer` → 默认值白名单语义(删掉)、`isExtracted` → `extraction_id`,frontier 用 `created_at`,认领走单事务。压缩触发的 session 内提取在这套列上同样成立(按 session 过滤、不设 5 条门槛、同样置 `extraction_id`),与全局节拍的去重天然打通。

---

## 问题 3（用户）

“turn 1 还在跑,turn 2–6 都完成了——计数=5,会触发,提取 2–6,把 1 跳过去了。” 这个我要纠正一下，这个不违反我的要求！如果出现这样的情况，turn1还没完成，先把2-6拿去提炼是可以的！后面7-10完成了，再把1，7，8，9，10进行提炼，是合理的！如果这样，你还觉得有什么问题吗？

### 回答

这个澄清很关键——去掉"连续性"约束后,方案大幅简化,之前我列的坑有一半直接消失了。重新盘点一下:

## 随 frontier 一起消失的问题

- **跨 session 全局序**:不再需要。触发条件退化为纯计数:`已完成 ∧ 未提取的锚点行 ≥ 5`,不关心它们之间隔着谁。`created_at` 平局、毫秒级模糊这些顾虑全部作废。
- **僵尸 turn 阻塞**:之前 `running` 卡死 frontier 是最大的工程负担(超时清扫、启动清扫、`abandoned` 状态)。现在一个崩溃的 turn 只是永远停在未完成,**不阻塞任何人**——它自己不被计数、不被提取(内容本来就残缺,跳过是合理行为),仅此而已。清扫机制从"必需"降级为"可选的可观测性优化"。
- **漏触发兜底也更简单**:计数只通过 turn 完成事件增长,每次完成都评估一次,跨过 5 的那次必然触发;就算因 DB 抖动错过一次,计数停在 ≥5,下一个 turn 完成时照样补上(配个 3N 的批次上限即可)。

你举的时序(2–6 先提,1 随 7–10 后提)在这套规则下自动成立,不需要任何额外逻辑。

## 仍然成立的问题(与 frontier 无关)

只剩三个,都是之前说过的:

1. **锚点识别仍然不能靠 `isTurnDone` + `isSteer` 两个布尔**。这一点和 frontier 无关,是范围推导的需要:提取时一个 turn 的消息范围 = `[本锚点.seq, 同session下一个锚点.seq)`,所以必须能识别"哪些 user 行是锚点"。`isSteer` 只标了 steer,但 `InjectGroupMessage` 注入的群聊 bot 消息、`goal_context` 续跑消息也是 user role 且都不是锚点——黑名单标不全。坚持之前的白名单写法:一个 `turn_status` 字段,默认 `''`(非锚点),只有 turn 开始路径写 `'running'`,完成时翻成 `'done'`。枚举现在只需要这三个值,`failed/abandoned` 可以不要了。

2. **认领事务一行不能少**。两个并发 turn 同时完成、同时数到 5,依然会双触发。"`SELECT` 数够 5 → `UPDATE` 置位"必须在同一个写事务里(SQLite `BEGIN IMMEDIATE` / Postgres `FOR UPDATE`),这是整个方案防重的唯一支点,跟表结构、frontier 都无关。

3. **`AppendMessage` 要返回 `seq`**,turn 结束时才能定位回自己的锚点行做 UPDATE。

另外 `isExtracted` 布尔 vs `extraction_id` 的取舍不变:布尔够用,代价是提取失败时要么补偿重置、要么丢一批只记日志;`extraction_id` 列成本相同,留个重试/审计的口子,我仍倾向后者,但这是品味问题不是正确性问题。

## 最终方案(修正版)

```sql
ALTER TABLE session_messages ADD COLUMN turn_status   TEXT NOT NULL DEFAULT '';
   -- '' = 非锚点(steer/群聊注入/goal_context/历史存量) | 'running' | 'done'
ALTER TABLE session_messages ADD COLUMN extraction_id TEXT;   -- NULL = 未提取
CREATE INDEX idx_sm_pending ON session_messages (agent_id, chatter_user_id)
    WHERE turn_status = 'done' AND extraction_id IS NULL;
```

- **turn 开始**:归档 user message 时带 `turn_status='running'`,记下返回的 seq;
- **turn 结束**(`runPostTurn`):翻成 `'done'`,然后在单事务里数 `done ∧ 未提取`,≥5 则生成 uuid 认领(`LIMIT 15` 封顶),提交后异步提取;
- **提取输入**:按认领行从归档表推导各 turn 范围,按 (session, seq) 排序拼 prompt;
- **压缩前提取**(未来):同一套列,加 `session_key=?` 过滤、去掉 ≥5 门槛,认领方式相同,与全局节拍天然互斥去重。

结论:你的纠正成立后,这个加列方案我没有保留意见了——它在正确性上和双表方案等价,工程量小得多。剩下的三条(白名单锚点、认领事务、返回 seq)是实现时的硬要求,不是方案缺陷。

---

## 问题 4（用户）

你说的三条，我只有第二条看懂了，就是说select和update要在一个事务里执行原子化避免并发问题。但是第一条和第三条没太懂，请你用通俗易懂的方式讲解一下。

### 回答

没问题,这两条都是实现层面的细节,我用具体例子讲。

## 第一条:为什么不能只靠 `isSteer` 来识别"锚点"

**先说什么是锚点**。我们的设计里,一个 turn 在数据库里没有"从哪到哪"的范围字段,turn 的边界是**推导**出来的:

> 一个 turn 的全部消息 = 从它的 user message 开始,到同一个 session 里**下一个能代表 turn 的 user message** 为止。

这个"能代表 turn 的 user message"就是我说的锚点。所以提取时,程序必须能准确回答一个问题:**这一行 user 消息,是不是一个 turn 的开头?**

问题在于,`session_messages` 里 `role='user'` 的行不止两种。看一个真实会话可能的样子:

| seq | role | 内容 | 它是什么 |
|-----|------|------|----------|
| 0 | user | "帮我查下天气" | ✅ turn 1 的锚点 |
| 1 | assistant | (调工具) | turn 1 的一部分 |
| 2 | tool | 天气数据 | turn 1 的一部分 |
| 3 | user | "顺便用摄氏度" | steer 插话,turn 1 的一部分 |
| 4 | assistant | "今天 25°C…" | turn 1 的结尾 |
| 5 | user | "\[小明\]: 大家好" | 群里**另一个 bot** 说的话,被 `InjectGroupMessage` 塞进来"让 agent 知道",**没有触发任何 turn** |
| 6 | user | "明天呢?" | ✅ turn 2 的锚点 |

你的方案用 `isSteer` 能把 seq=3 排除掉,但 **seq=5 怎么办?** 它不是 steer,你给它 `isSteer=false`,于是它看起来就像一个锚点——而且它永远不会有 turn 结束事件,`isTurnDone` 永远是 false,看起来像一个"还在跑的 turn"。更糟的是范围推导会切错:turn 1 的范围本该是 seq 0–5,现在被 seq=5 拦腰截断。除了群聊注入,还有 goal_context 续跑消息,以后可能还有新的注入类型——**用"黑名单"思路,你得给每一种非锚点消息都发明一个标记字段,漏一种就出 bug**。

所以换成"白名单"思路:`turn_status` 默认是空字符串 `''`,意思是"我不是锚点"。**全代码库只有一个地方**——`HandleMessage` / `HandleMessageStream` 里真正开启一个 turn、写入用户消息的那一行代码——会显式写 `turn_status='running'`。steer、群聊注入、goal_context、以及未来任何人新加的注入路径,什么都不用做,默认就是 `''`,自动被排除。安全性靠默认值兜底,而不是靠每个开发者记得打标记。

顺带,这样 `isSteer` 字段就不需要存在了,`isTurnDone` 也被吸收进来:`''`(非锚点)/ `'running'`(锚点,turn 进行中)/ `'done'`(锚点,turn 完成)——一个字段表达了你原来两个布尔想表达的事,还多覆盖了你没想到的情况。

## 第三条:为什么 `AppendMessage` 要返回 `seq`

这条纯粹是管道问题。流程是:

1. **turn 开始**:程序把用户消息写进 `session_messages`,带上 `turn_status='running'`;
2. **turn 结束**(`runPostTurn`):程序要把**刚才那一行**改成 `'done'`。

第 2 步是一条 UPDATE,UPDATE 需要 WHERE 条件精确锁定那一行。这张表的主键是 `(user_id, agent_id, session_key, seq)`——前三个程序都知道,**唯独 `seq` 不知道**。

为什么不知道?看现在的插入逻辑(`database.go` 的注释里写了):`seq` 是 INSERT 的时候由数据库用 `COALESCE(MAX(seq), -1) + 1` **当场算出来**的,而现在的 `AppendMessage` 函数签名长这样:

```go
func (a *StoreAdapter) AppendMessage(...) error   // 只返回 error
```

数据库算出了 seq=42,但这个数字没有传回 Go 代码,调用方写完就两眼一抹黑。到 turn 结束时,它想改"自己那行",却说不出那行的主键。

解决办法就是把签名改成:

```go
func (a *StoreAdapter) AppendMessage(...) (seq int, err error)   // 把分配到的 seq 带回来
```

turn 开始时程序把返回的 `seq` 存在本次 turn 的局部变量里(turn 是一个函数调用的生命周期,放局部变量正合适),turn 结束时用它发精确的 UPDATE。改动很小,但不改就没法精确回写。

> 你可能会想:不返回 seq,结束时查"这个 session 里 `turn_status='running'` 的那行"不也行吗?大多数时候行(一个 session 同时只有一个 turn 在跑),但它依赖隐式约定:如果上次 daemon 崩溃在这个 session 留下了一行僵尸 `'running'`,这个查询就会找到两行,可能把僵尸行错误地标成 `'done'`,让一个残缺 turn 混进提取批次。拿着主键去改,不存在认错行的可能。两者实现成本几乎一样,精确的那个显然更好。

总结一下:第一条是说**"是不是 turn"要靠默认值白名单来判定,而不是给每种例外打补丁**;第三条是说**写入时要把数据库分配的行号带回来,结束时才能精确改回那一行**。都不复杂,但都是不做就会出 bug 的点。

---

## 问题 5（用户）

为什么会有seq=5这种群里其他机器人说话的情况？当前的fastclaw还有这种功能？我只知道有 steer功能啊！这个其他机器人说的话为啥要发给我的agent啊！这不胡闹吗，这个是意义是什么？

### 回答

我去代码里把这个功能的来龙去脉查清楚再回答你。

---

### 回答

不是胡闹,这是 FastClaw 已有的**群聊多 agent 上下文感知**功能。我把调用链查了一遍,给你讲清楚它存在的意义。

## 这个功能在哪、干什么

`internal/gateway/routing.go` 的群聊路由逻辑(233–293 行)支持**把多个 agent 绑定到同一个群**。一条群消息进来时,路由规则是:

- **有人 @了某个 agent** → 只有被 @的那个 agent 真正跑 turn 回复;但消息会通过 `InjectGroupMessage` **同时塞进其他所有绑定 agent 的 session 历史里**(routing.go:259–263);
- **没人 @,行为是 default-agent** → 默认 agent 回复,其他 agent 同样只注入不回复(routing.go:280–284);
- **没人 @,也没有默认 agent** → 谁都不回复,但**所有**绑定 agent 都注入这条消息(routing.go:289–291);
- **消息本身是另一个 bot 发的**(`msg.IsBotMessage`,比如群里绑了你的两个 agent,A 刚回复完,A 的回复对 B 来说就是"别的机器人说话") → 注入给所有 agent,如果还 @了谁,被 @的才被触发(routing.go:241–255)。

`InjectGroupMessage`(loop.go:583)做的事非常轻:把消息以 `\[发送者名\]: 内容` 的格式 append 进 session 历史,**不调用 LLM、不产生任何回复、不花一分钱 token**。

## 意义是什么

想象一个群里绑了你的两个 agent:"小助理"和"翻译官",再加几个真人:

```
真人甲: 我们下周三去东京出差
小助理: 好的,我记下了 ✈️
真人乙: @翻译官 帮我把刚才的安排翻成日语
```

轮到"翻译官"说话时,它需要知道"刚才的安排"是什么——可它之前一次都没被 @过。如果没有注入机制,它的 session 历史是**空的**,只能回一句"请问你说的安排是指什么?"。正是因为前面每条消息(包括"小助理"这个机器人的回复,这就是你问的 seq=5)都被悄悄注入了它的历史,它才能直接给出正确的翻译。

一句话:**群里每条消息只有一个 agent"开口",但所有 agent 都得"竖着耳朵听",否则下次被点名时就是个失忆的局外人**。其他 bot 的发言也得听,因为那同样是对话的一部分——你不会希望翻译官翻译时漏掉小助理刚说的内容。

## 对我们方案的影响

你平时只用 DM 或单 agent,所以从没感知到这条路径——但它真实存在,而且**注入的行 `role='user'`、不触发 turn、永远没有"turn 结束"事件**,和 steer 一样是"长得像 turn 锚点但不是"的行。这正是我坚持用白名单(`turn_status` 默认 `''`,只有真正开 turn 的那一处代码写 `'running'`)而不是 `isSteer` 黑名单的原因:今天有 steer 和群聊注入两种"假锚点",明天代码库再加第三种注入路径时,白名单方案一行代码都不用改,黑名单方案就会悄悄出 bug。

---

## 问题 6（用户）

群聊注入只注入其他机器人发送的消息吗？还是所有群聊记录都会注入，包括用户们的消息和机器人们的消息？

### 回答

都会注入——群里**每一条消息**(真人的和机器人的)最终都会进入**每一个**绑定到该群的 agent 的 session 历史,区别只在于"以什么方式进入"。把 routing.go 那段逻辑按消息来源拆开看:

**真人发的消息**(routing.go:257–292):

- 有人 @了 agent X → X 把这条消息当作**真正的 turn**(走 LLM、回复),其他 agent 通过 `InjectGroupMessage` **注入**(259–263 行,注意条件 `ag.Name() != target.Name()`,回复者自己不重复注入);
- 没 @人、配了默认 agent → 默认 agent 跑 turn,其余注入(280–284 行);
- 没 @人、没默认 agent → **所有** agent 都只注入,谁也不回复(289–291 行)。

**机器人发的消息**(routing.go:241–255):

- 先无条件注入给**所有**绑定 agent(242–244 行);
- 如果这条 bot 消息还 @了某个 agent,被 @者再额外被触发一个真 turn。

所以正确的心智模型是:**群聊的完整对话流会被复制到每个绑定 agent 各自的 session 里**。每条消息恰好有一个"处理方式"的分叉——被选中的回复者以 turn 的形式收到(`role='user'` 且触发 LLM),其他旁听者以注入的形式收到(`role='user'` 但不触发任何东西)。我上一轮只强调了"其他机器人的话"是因为那是最反直觉的一种,但真人消息同样会以注入形式出现在非回复者的历史里。

这对我们的方案意味着:在多 agent 群聊场景下,一个 agent 的 `session_messages` 里**注入行可能远多于锚点行**——群里 90% 的消息它都只是旁听。这进一步说明为什么锚点识别必须靠白名单:对这种 session,"user role 行数"和"turn 数"完全是两个量级,任何基于 user 行计数的逻辑(包括现状的 `CountChatterUserMessages`)在群聊里偏差会非常大。

顺带说一个我在核对时注意到的现有代码小毛病(与本方案无关,但你可能想知道):bot 消息的分支是**先给所有 agent 注入、再触发被 @者**(242 行注入时没有排除 target),所以被 @的 agent 会先收到一份注入副本、又收到一份 turn 副本,同一条消息在它的历史里出现两次。真人消息的分支就正确地排除了回复者。如果哪天你看到 agent 复述"刚才有人说了两遍",根源在这里。

---

## 问题 7（用户）

那这种群聊agent的session，我在web ui上是能看到这个session，并发消息的吗？还是说群聊agent的session是隐藏的？以及群聊的session会更新吗，否则这个session岂不是无限膨大？

### 回答

好问题,这涉及 web UI 的 session 列表逻辑和压缩触发时机。我去查一下代码再回答。

---

### 回答

查完了,两个问题分开答:

## 1. Web UI 上能看到群聊 session,也能往里发消息

**能看到**:侧边栏的 session 列表走 `StoreAdapter.ListWebSessions`(store_adapter.go:212–220),注释写得很明确——"returns every chat session for this agent **regardless of channel**",函数名里的 "Web" 只是历史遗留。telegram/discord/微信的群聊 session 会和 web 会话混排在 dashboard 侧边栏里,带渠道图标前缀。历史记录走 `WebChatHistory`(loop.go:915 起),同样服务所有渠道,而且读的是 append-only 归档表,所以你能看到完整群聊记录(包括注入的旁听消息——不过 UI 层只渲染 `Origin == OriginUser` 的行,goal_context 这类合成行被隐藏)。

**能发消息**:这是刻意支持的双向能力。`recoverWebTriple`(loop.go:524–562)的整段注释就是在处理"从 web UI 往一个 telegram/微信 session 里写消息"的场景——它把 URL 里的 session token 反解成 `(channel, accountID, chatID)` 原始三元组,保证你在 web 上发的消息、agent 的回复,都落进**同一个** session,刷新后不丢。也就是说你可以在 web 上"私下"和这个群聊 agent 对话,共享群聊的上下文。(一个值得留意的边界:这种 turn 里如果模型主动调用 `message` 工具,`bindSession` 给它绑的是原始渠道的 (channel, chatID),消息可能真的发进群里——这是把双刃剑,看你怎么用。)

## 2. 会膨胀,而且其中一层的膨胀比你想的更严重

session 有两层存储,命运不同:

**归档层(`session_messages` 表)**:append-only,**永不压缩、无限增长,这是设计意图**(建表注释:"the original full history so UI / audit / multi-tenant recovery has a source of truth that compaction never touches")。群聊和私聊都一样。这是磁盘问题,不是 token 问题,量级上可以接受,但群聊确实会让它涨得快得多。

**工作集(发给 LLM 的那份,`sessions` blob + 内存)**:这才是你担心的点,而且现状确实有问题。压缩(`CompactMessages`,80K token 阈值)**只在真 turn 开始时执行**(loop.go:1851 / 2554,`sess.Append(userMsg)` 之后)。而 `InjectGroupMessage` 走的是裸 `sess.Append`(loop.go:590),只追加、不检查。推论:

- 只要 agent **偶尔被 @一次**,下一个真 turn 开始时压缩就会把积累的旁听消息折叠掉——所以是"懒压缩",有界,只是边界是"距上次被触发之间的增量";
- 但一个**长期没人 @的纯旁听 agent**,工作集会无限膨胀:没有任何代码路径触发它的压缩。等到几个月后终于有人 @它,那一刻它会带着一个巨大的工作集先做压缩(一次大的 LLM 摘要调用),才能开始回答。
- 还有个伴生的写放大问题:`Session.Append`(manager.go:448–449)每追加一条消息都把**整个工作集 blob 重写一遍**(`SaveSession` 全量保存)。一个活跃大群里,每条群消息都触发一次 O(session 大小) 的写——session 越膨胀,每条旁听消息的写入成本越高,这是平方级的累积写放大。

所以诚实的回答是:**会更新,但只在被触发时才更新;纯旁听场景下工作集无限膨胀是现状的真实缺陷**。如果要修,方向很直接:`InjectGroupMessage` 里加一个廉价的本地检查(消息条数或估算 token 超阈值就同步裁剪,甚至不需要 LLM 摘要,旁听消息直接掐头保尾就够了),顺带也缓解写放大。这和我们的记忆提取方案是正交的——提取读的是归档层,不受工作集裁剪影响,这也正是方案把输入源从内存工作集切换到归档表的另一个好处。

---

## 问题 8（用户）

这个session_messages，平台上所有用户的所有session的消息全部都记录在这一个表里，甚至有的session还是群聊session，这个设计合理吗？到后面整个平台上的所有消息全在这一张表里，变成一个超级无敌巨大的表，不太好吧？查询之类的表操作也会变慢吧！

### 回答

这个担心很常见,但结论可能和直觉相反:**"全平台消息放一张(逻辑)表"恰恰是行业标准做法,而它的反面——按用户或按 session 分表——才是公认的反模式**。我分三层说:

## 1. 为什么大表本身不是问题:查询慢不慢取决于索引,不取决于行数

`session_messages` 的主键是 `(user_id, agent_id, session_key, seq)`,另有 `(chatter_user_id, agent_id, session_key, seq)` 的部分索引。看现在所有的访问路径——拉某个 session 的历史、查某个 chatter 的消息、按 seq 范围回放——**全部是主键前缀上的点查/范围查**,走 B-tree。B-tree 查找是 O(log n):100 万行大约 20 层比较,10 亿行大约 30 层,**行数翻一千倍,单次查询成本只多 50%**。只要没有全表扫描型的查询,这张表涨到几十亿行,点查依然是毫秒级。

Slack、Discord、Telegram 的消息存储,逻辑上也都是"一张大消息表"(物理上做了分片,见第 3 点),没有谁按聊天室建表。

## 2. 分表才是真正的坑

假如改成每用户/每 session 一张表,你会立刻遇到:

- **schema 迁移地狱**:加一列(比如我们方案里的 `turn_status`)要对几万张表执行 ALTER,任何一张失败就是不一致状态;
- **跨表查询消失**:"这个 chatter 在所有 session 的消息"这种现有查询(`CountChatterUserMessages`、记忆提取认领)变成动态拼表名的循环查询;
- **数据库目录本身成为瓶颈**:PG 的 catalog、SQLite 的文件句柄,几十万张表会先于数据量把系统拖垮;
- 备份、权限、监控全部碎片化。

数据库就是为"少量大表 + 好索引"设计的,不是为"海量小表"设计的。

## 3. 真到了平台级体量,标准演进路径早就铺好了——而且这张表的设计恰好对此友好

行数大到运维难受(备份慢、索引内存放不下、冷数据占着昂贵存储)时,业界的标准动作依次是:

1. **保留策略(retention)**:这是最该先做的,而且和你上一轮发现的问题直接相关——群聊旁听注入的消息是天然的"低价值高体积"数据,完全可以定 90 天 TTL 定期清掉,真正的 turn 消息保留更久。一条 DELETE 策略能砍掉大半体积;
2. **分区(partitioning)**:PG 原生支持按 `created_at` 做时间分区或按 `hash(user_id)` 分区,**对应用代码完全透明**,旧分区可以整体 DETACH 归档到对象存储;
3. **分片(sharding)**:再往后按 user_id 把用户整体路由到不同库。

关键是:这三步都**不需要改逻辑 schema**。`user_id` 在主键第一位、每条查询天然带租户前缀,这正是日后做分区/分片的理想形状——这张表的多租户设计其实是为这条路预留好的。

## 真正值得担心的不是表大,而是这两点

- **SQLite 后端**:它是单写者,平台级并发写入会先在"同时只能一个事务写库"上撞墙,远早于表大小成为问题。所以平台化部署必须用 PG 后端(代码里 `d.ph()` 占位符抽象表明双后端已支持),SQLite 留给单机模式;
- **上一轮说的写放大**:`Session.Append` 每条消息全量重写 `sessions` 工作集 blob,这是 O(session 大小) 的写,在活跃大群里是平方级累积——这个的实际伤害会比 `session_messages` 的行数增长来得早得多、狠得多。

所以我的判断:**单表设计本身合理,不要动;优先级在于群聊消息的 retention 策略和 Append 写放大,真到亿级行量再上 PG 分区,一步都不用提前做**。过早分表是在为一个 O(log n) 的"问题"引入一堆 O(n) 的真问题。

---

## 问题 9（用户）

当前这个会话的内容我想分享给另一个session看，分享链接的方式不可以，我试过了，另一个session的agent访问不了。你看是输出md文档还是怎么样，又或者说这些对话数据是存在本机的哪个文件夹下的吗？给我路径我直接让另一个session的agent去找。

### 回答

Claude Fable 5 is currently unavailable. Learn more: https://www.anthropic.com/news/fable-mythos-access

---

## 问题 10（用户）

当前这个会话的内容我想分享给另一个session看，分享链接的方式不可以，我试过了，另一个session的agent访问不了。你看是输出md文档还是怎么样，又或者说这些对话数据是存在本机的哪个文件夹下的吗？给我路径我直接让另一个session的agent去找。

### 回答

你想要的是这个 Claude Code 会话本身的存档路径,我去本机找一下它存在哪。

---

### 回答

找到了。这个会话的原始存档就在本机:

```
~/.claude/projects/-home-user-fastclaw/e0bc89c9-9012-5355-b5de-2eabdd6ba001.jsonl
```

但在让另一个 agent 去读它之前,有个关键点你得知道——这很可能正是分享链接失败的同一个原因。我先确认文件内容,再给你一个更可靠的方案。

---

### 回答

确认了,这就是我们当前会话,104 行。现在说重点:

**为什么本机路径大概率没用(也是分享链接失败的原因)**:你这个会话跑在一个**远程、隔离、用完即弃的云容器**里。"另一个 session"如果是另一个 web 会话,它会在**另一个独立容器**里启动,看不到这个容器的 `~/.claude/`,容器闲置后还会被回收。所以给路径这条路,只有在两个 agent 恰好同一个容器里时才成立——通常不成立。

**可靠的通道是 git 仓库本身**:两个 session 都会克隆同一个仓库。我把这次对话导出成一份干净的 markdown 放进仓库,你提交推送后,另一个 session 拉下来就能读。我现在就生成。

---

