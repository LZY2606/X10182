# gval 表达式端到端追踪分析

本文选取一条同时包含「伪装 token（camouflage）」「不同优先级二元运算」「短路布尔」「动态下标」与「自定义 selector」的表达式，从 `NewEvaluable` 一直追到最终值/错误，并逐条引用当前 commit 的源码 `file:line`。

被追踪的主表达式（定义于 `observable_trace_test.go:20` 的 `masterExpression`）：

```
2 * 3 + 4 == 10 && probe.items[idx] > 5 || probe.secret == 99
```

配套的可观测样本在 `observable_trace_test.go`：记录型 selector `pathRecorder`（`observable_trace_test.go:33`）实现 `gval.Selector`，每次 `SelectGVal` 都把「访问的 key」与「当下 context 的错误状态」记入 `visits`（`observable_trace_test.go:41`）；可运行示例 `Example_observableTrace`（`observable_trace_test.go:78`）打印最终值与访问轨迹。

运行时根参数是一个 `*pathRecorder`：

```go
root  = &pathRecorder{data: {"probe": child, "idx": 1.0}}
child = &pathRecorder{data: {"items": []interface{}{3,7,9}, "secret": 99.0}}
```

最终结果：值为 `true`，错误为 `nil`；根 selector 的访问序列为 `[probe idx]`，内层 `probe` selector 的访问序列为 `[items]`，`secret` 因短路完全不被访问（见 `observable_trace_test.go:79` 的 `// Output:`）。

---

## 1. 入口：`NewEvaluable` 到 parser

调用链：

- `gval.Full()` 返回组合语言 `full`，它在 `var full = NewLanguage(arithmetic, bitmask, text, propositionalLogic, ljson, ...)`（`gval.go:121`）中由各子语言合并而成；公开包装 `Full()` 直接返回该单例（`gval.go:39-44`）。
- `Evaluate(expr, parameter)`（`gval.go:20`）用 `context.Background()` 进入 `EvaluateWithContext`（`gval.go:21`、定义 `gval.go:25`）。
- `Language.EvaluateWithContext`（`language.go:88`）先 `NewEvaluableWithContext`（`language.go:89`）拿到可执行体，再在 `language.go:93` 用 `eval(c, parameter)` 执行；执行期错误在 `language.go:95` 包成 `can not evaluate ...: %w`。
- `NewEvaluable`（`language.go:62`）只是 `NewEvaluableWithContext(context.Background(), ...)`（`language.go:63`）。
- `NewEvaluableWithContext`（`language.go:67`）构造 parser（`language.go:68`），调用 `p.parse(c)`（`language.go:70`）。parse 成功后，若 parser 仍处于伪装态且最后扫描的不是 EOF，则把伪装错误提升为真正的解析错误（`language.go:71-73`）；否则在 `language.go:76` 包成 `parsing error: ... - line:col %w`。这一步是 camouflage 错误唯一的「总出口」。
- `p.parse`（`parse.go:80`）没有自定义 `init`，直接进入 `ParseExpression`（`parse.go:85`）。

`ParseExpression`（`parse.go:14`）是一个调度循环：每轮先用 `ParseNextExpression` 解析一个操作数（`parse.go:26`），再用 `parseOperator` 解析其后缀/二元算符（`parse.go:31`），然后把 stage 压入优先级栈（`parse.go:33`）。当栈顶 stage 没有 `infixBuilder`（说明后面不是算符）时，弹出唯一的 `Evaluable` 作为本层结果（`parse.go:37-38`）。

`ParseNextExpression`（`parse.go:44`）用 `p.Scan()` 取一个 rune（`parse.go:45`），在 `p.prefixes[scan]` 里分派前缀规则（`parse.go:46`、`parse.go:53`）。数字走 `parseNumber`（`gval.go:283` 注册，实现 `parse.go:96`），标识符走 `PrefixMetaPrefix(scanner.Ident, parseIdent)`（`gval.go:279`）。

---

## 2. camouflage 如何被判断与「进位」

gval 的词法器是 go 的 `text/scanner`，parser 用一套「多扫一个 token，再假装没扫」的回溯机制来判断一个 token 到底属于算符还是变量/括号的结束符。

### 2.1 伪装的状态机

- `Scan()`（`parser.go:63`）：若当前处于伪装态 `isCamouflaged()`，它不真正推进扫描器，而是清掉 `camouflage` 并把上一次保存的 `p.lastScan` 原样返回（`parser.go:64-67`）——这就是「进位（carry）」：被伪装（回退）的 token 在下次 `Scan` 时被重新读出。否则真正调用底层扫描器并记录到 `p.lastScan`（`parser.go:68-70`）。
- `isCamouflaged()` 要求 `camouflage` 非空且不是 `errCamouflageAfterNext`（`parser.go:73-75`）。
- `Camouflage(unit, expected...)`（`parser.go:80`）设置 `p.camouflage = p.Expected(...)`（`parser.go:84`），即把「期望什么、实际扫到什么」保存下来；连续两次 `Camouflage` 会 panic（`parser.go:81-83`），保证它只紧跟在一次 `Scan` 之后。

### 2.2 主表达式里的两次 camouflage

1. 变量结束符 `]`（以及后续 `>`、`==`）。解析 `probe.items[idx]` 时，`parseIdent` 读到 `[` 后递归解析下标表达式 `idx`（`parse.go:202-206`），随后 `p.Scan()` 期望 `]`（`parse.go:207`）。读到的 `]` 满足 case，于是只把下标 `idx` 追加到路径 keys（`parse.go:208-209`），循环继续：下一次 `p.Scan()`（`parse.go:185`）扫到 `>`。`>` 既不是 `.`、`(`、也不是 `[`，落入 `default`（`parse.go:213`），调用 `Camouflage("variable", '.', '(', '[')`（`parse.go:214`）并返回 `p.Var(keys...)`（`parse.go:215`）。也就是说 `>` 被「回退」，交还给上层 `parseOperator` 重新识别为二元算符。下标括号内真正结束子表达式的那次 camouflage 发生在 `idx` 之后：`ParseExpression` 内部的 `parseOperator` 扫到 `]`（非 symbol、非 Ident），在 `parse.go:138-140` 执行 `Camouflage("operator")` 并把无算符的 stage 返回；这个 `]` 随即由 `parse.go:207` 的 `p.Scan()` 进位读出并确认。
2. 结尾的 EOF。最后 `parseIdent` 在处理 `probe.secret` 后，`p.Scan()` 扫到 EOF（`parse.go:185`），同样落入 `default`，`Camouflage("variable", ...)` 后返回变量（`parse.go:213-215`）。随后 `parseOperator` 再次 `Scan`（`parse.go:127`）进位得到 EOF；EOF 不是 symbol，且 `scan != scanner.Ident`，于是在 `parse.go:138-140` 再次 `Camouflage("operator")`，`ParseExpression` 看到栈顶无 `infixBuilder` 而正常返回（`parse.go:37-38`）。由于最后扫描的是 EOF，`NewEvaluableWithContext` 的 `p.lastScan != scanner.EOF` 条件不成立（`language.go:71`），所以这个伪装错误不会被抛出。
3. 多字符 symbol 算符还有「贪心跳过、错了就报未知算符」的分支：`parseOperator` 在 symbol 情况下 `Peek` 下一个字符，只要 `isOperatorPrefix(op+next)` 成立就 `Next()` 并扩展 op、置 `mustOp=true`（`parse.go:130-137`）。只有当 `mustOp` 为真却没匹配到任何算符时，才在 `parse.go:173` 报 `unknown operator`；否则仍可 `Camouflage("operator")`（`parse.go:169-171`）。这保证 `*` 能继续拼成 `**`，而单独的 `>` 不会被强行当成算符前缀。

---

## 3. 优先级：何时压栈、何时出栈

优先级在 `base` 里用 `Precedence(name, n)` 注册（值越大越先算），本式用到的有（`gval.go`）：

| 算符 | 优先级 | 定义 |
| --- | --- | --- |
| `*` | 150 | `gval.go:329` |
| `+` | 120 | `gval.go:326` |
| `==` `>` | 40 | `gval.go:309`、`gval.go:311` |
| `&&` | 21 | `gval.go:307` |
| `||` | 20 | `gval.go:306` |

机制是单调递增的算子栈 `stageStack`（`operator.go:19` 注释明确「precedence 单调上升」）。核心在 `stageStack.push`（`operator.go:21`）：只要栈顶优先级 `>=` 新算符优先级，就弹出栈顶 `a`（`operator.go:23`），用 `a.infixBuilder(a.Evaluable, b.Evaluable)` 组装成新的右值 `eval`（`operator.go:24`），并用它替换 `b.Evaluable`（`operator.go:36`）后继续比较；直到栈顶更松，再把 `b` 压栈（`operator.go:38`）。`>=`（而非 `>`）决定了同级算符**左结合**。`parseOperator` 识别到 `*infix` 时把 `operator.builder` 连同优先级一起放进 stage（`parse.go:142-148`）。

下表按 `ParseExpression` 的迭代，逐步推演主表达式（用 `C(x)` 表示常量、`Var(path)` 表示变量子树）：

1. 读 `2` → stage `C(2)`（无 builder）；`parseOperator` 扫到 `*`（优先级 150），push：栈 `[2 @*]`。
2. 读 `3`；扫到 `+`（120）。push `[3 @+]`：栈顶 `*`150 `>=` `+`120，弹出组装 `2*3`（这里两侧皆常量，见 §4 被折叠成 `C(6)`）；栈空后压入，栈 `[C(6) @+]`。
3. 读 `4`；扫到 `==`（40）。push `[4 @==]`：`+`120 `>=` 40，弹出组装 `C(6)+4` → 折叠成 `C(10)`；栈 `[C(10) @==]`。
4. 读 `10`；扫到 `&&`（21）。`==`40 `>=` 21，组装 `C(10)==C(10)` → 折叠成 `C(true)`；栈 `[C(true) @&&]`。
5. 经 `parseIdent` 得到变量子树 `Var(probe.items[idx])`（`parse.go:215`）；`parseOperator` 进位扫到 `>`（40）。push 变量 `@>`：栈顶 `&&`21 `<` 40，不出栈，直接压入；栈 `[C(true) @&&, Var(probe.items[idx]) @>]`（保持单调上升）。
6. 读 `5`；扫到 `||`（20）。push `[5 @||]`：先弹出 `>`40，组装 `Var(probe.items[idx]) > 5`（含变量，**不折叠**，`operator.go:28` 条件不满足）；再弹出 `&&`21 `>=` `||`20，组装 `C(true) && (Var>5)`；最后压入，栈 `[... @||]`。
7. 经 `parseIdent` 得到 `Var(probe.secret)`；`parseOperator` 进位扫到 `==`（40）。`||`20 `<` 40，压入，栈 `[... @||, Var(probe.secret) @==]`。
8. 读 `99`；`parseOperator` 进位扫到 EOF，伪装后返回无 builder 的 stage。push 时先弹出 `==`40 组装 `Var(probe.secret)==C(99)`，再弹出 `||`20 组装最终树：

```
||(
  &&( C(true),  >( Var(probe.items[idx]), C(5) ) ),
  ==( Var(probe.secret), C(99) )
)
```

至此 parse 完成，`NewEvaluableWithContext` 返回该 `Evaluable`（`language.go:79`）。

---

## 4. 常量子树为何能在解析期提前求值

折叠只发生在「左右两个操作数都是 parser 常量」这一严格条件下：

- `stageStack.push` 组装出 `eval` 后，检查 `a.IsConst() && b.IsConst()`（`operator.go:28`）。若为真，立即用 `eval(nil, nil)` 在**解析期**求值（`operator.go:29`，此时参数/context 都传 `nil`），成功后把结果重新包成 `constant(v)`（`operator.go:33`）并 `continue` 继续向上折叠（`operator.go:34`）。任何求值错误都直接作为解析错误返回（`operator.go:30-32`）。
- 数字字面量是常量：`parseNumber` 返回 `p.Const(n)`（`parse.go:101`），字符串同理（`parse.go:93`）。
- `IsConst()` 不是类型标记，而是比较函数指针是否等于唯一的 `constant` 闭包（`evaluable.go:298-301`）；`constant` 用 `//go:noinline`（`evaluable.go:80-85`）保证指针身份稳定。因此普通变量 `Var(...)`（`evaluable.go:97-102`、`evaluable.go:120`）和函数调用闭包（`evaluable.go:227`）指针都不同，永远不会被当成常量。
- 前缀算符也有对称折叠：`PrefixOperator` 在 `eval.IsConst()` 时（`language.go:201`）于解析期执行一次 `prefix(c, nil)`（`language.go:202`）并替换为 `p.Const(v)`（`language.go:206`）。

因此 §3 中 `2*3`、`+4`、`==10` 三步在解析期就折叠成 `C(true)`，运行时不再涉及 `float64` 运算；而只要子树里夹了 `Var` 或函数调用，`operator.go:28` 的条件即为假，整棵子树保留到运行期（`operator.go:36`）。

**为什么折叠不会偷跑副作用**：能被折叠的子树只能由字面量与「已折叠结果」构成；自定义函数走 `Function`（`language.go:107`）生成的是 `callFunc` 闭包（前缀规则里的 `p.callFunc(toFunc(function), args...)` 在 `language.go:121`，闭包定义 `evaluable.go:227`），它不是 `constant`，`IsConst()` 为假，所以 `1 + boom()` 这种表达式在 parse 时只构建闭包、不执行 `boom`。样本 `TestObservableConstantFoldingNoSideEffect/function_side_effect_not_run_at_parse`（`observable_trace_test.go:179`）断言：`NewEvaluable` 之后调用计数为 0，真正 `Evaluate` 后才为 1。反证折叠确实发生在解析期的样本是 `1 - "x"`：它两侧皆常量，折叠时执行减法得到类型错误，错误以 `parsing error: ... invalid operation (float64) - (string)` 在 `NewEvaluable` 阶段返回（断言见 `observable_trace_test.go:171-174`；包装点 `language.go:76`）。

---

## 5. `&&`/`||` 如何阻止右侧 selector 被调用

`&&`、`||` 同时注册了短路判定与布尔运算（`gval.go:263-266`）：

- `&&`：`InfixShortCircuit("&&", func(a){ return false, a == false })`（`gval.go:263`）；
- `||`：`InfixShortCircuit("||", func(a){ return true, a == true })`（`gval.go:265`）。

`infix.initiate` 在 `op.shortCircuit != nil` 时（`operator.go:105`）构建这样的执行体（`operator.go:106-120`）：

1. 只求左操作数 `a`（`operator.go:108`），出错即返回（`operator.go:109-111`）；
2. 调短路判定 `shortF(a)`，若第二个返回值为 `true`，**直接**返回第一个值，不再触碰右操作数（`operator.go:112-114`）；
3. 只有没短路时才求右操作数 `b`（`operator.go:115`）并做最终布尔运算 `f(a,b)`（`operator.go:119`）。

对照无短路算符的 builder 会无条件先求 `a` 再求 `b`（`operator.go:89-101`），差别就在 `operator.go:112-114` 这一次提前返回。

主表达式运行时（折叠后 `C(true)` 为左值）：

- `C(true) && (probe.items[idx] > 5)`：左值 `true`，`&&` 判定 `a==false` 为假，**不**短路，于是求值右侧（见 §6），`7 > 5` 为 `true`。
- 结果 `true` 再作为 `||` 的左值，`||` 判定 `a==true` 为真，立即返回 `true`（`operator.go:112-114`）。右操作数 `probe.secret == 99` 作为闭包虽已在解析期构建（§3 第 7-8 步），但运行时从未被调用，因此内层 selector 不会访问 `secret`——这正是示例输出里 `probe visits: [items]`（`observable_trace_test.go:90`）没有 `secret` 的原因。

样本 `TestObservableShortCircuit`（`observable_trace_test.go:113`）对 `false && probe.secret...`（用例定义 `observable_trace_test.go:122`）与 `true || probe.secret...` 两种情形都断言 `len(rec.visits) == 0`（`observable_trace_test.go:137-139`），即右侧 selector 完全不被访问。

---

## 6. 变量路径：根参数、静态段、动态段如何取值

`parseIdent` 把整条路径编译成一个 `Evaluables`（keys 序列），而不是每段一个独立变量：

- 首个标识符作为常量 key 放入 keys（`parse.go:183`，`p.Const(token)`）；
- `.field` 追加一个**静态**常量段（`parse.go:187-192`）；
- `[ expr ]` 把子表达式整体作为一个**动态**段追加（`parse.go:202-209`）；
- 最终一次 `p.Var(keys...)` 构造变量（`parse.go:215`）。

`Parser.Var`（`evaluable.go:97`）在未注入自定义 `VariableSelector` 语言（`language.go:289`）时返回内建的 `variable(path)`（`evaluable.go:99`、`evaluable.go:120`）。

`variable` 的运行期循环（`evaluable.go:121-155`）有两个关键事实：

1. **每一段的 key 都对原始根参数 `v` 求值，而不是对当前段对象 `v2` 求值**：`k, err := p.EvalString(c, v)`（`evaluable.go:124`），传入的是循环外固定的根 `v`（`evaluable.go:121-122`）。所以动态下标 `[idx]` 里的 `idx` 总是相对根参数解析，与父级对象无关。
2. **当前值 `v2` 才按类型分派**（`evaluable.go:128-152`）：
   - `Selector`：调用自定义 `o.SelectGVal(c, k)`，错误包成 `failed to select '<k>' on <T>: %w`（`evaluable.go:129-134`）；
   - `map[interface{}]interface{}` / `map[string]interface{}`：直接索引（`evaluable.go:135-140`），缺键得到 nil 且无错误；
   - `[]interface{}`：key 能 `Atoi`、非负且 `len(o) > i` 才取下标（`evaluable.go:141-145`），否则什么都不赋值（保持 `v2` 为原 slice），循环继续；
   - 其它类型（typed slice、typed map、struct、指针等）走 `reflectSelect(k, o)`（`evaluable.go:146-151`）；返回 `ok=false` 时报 `unknown parameter '<k>' on <T>`（`evaluable.go:150`）。

### 6.1 主表达式 `probe.items[idx]` 的逐段执行

根参数 `v = *pathRecorder(root)`，路径 keys = `[C("probe"), C("items"), Var(idx)]`：

- 段 `"probe"`：`v2` 是根 `*pathRecorder`，命中 `Selector`（`evaluable.go:129`）→ 记录 `probe`（context 正常），返回 `child`。
- 段 `"items"`：注意 key 仍对根 `v` 求值（`evaluable.go:124`），`"items"` 是常量段；`v2=child` 命中 `Selector` → 记录 `items`，返回 `[]interface{}{3,7,9}`。
- 段动态 `Var(idx)`：`p.EvalString(c, v)`（`evaluable.go:124`）用**根** `v` 重新跑一次变量循环——在根 recorder 上解析出 `data["idx"] = 1.0`，字符串化为 `"1"`（`EvalString` 对非字符串用 `fmt.Sprintf`，`evaluable.go:72`），所以根 recorder 记录 `idx`；随后 `v2` 当前是 `[]interface{}`，`Atoi("1")=1` 且在界内，取到 `o[1]=7.0`（`evaluable.go:142-144`）。

于是 `7 > 5` 为真。根 recorder 的访问序列是 `[probe, idx]`，内层 child 是 `[items]`；`idx` 出现在**根**记录里，直观证明了动态下标相对根对象求值。

### 6.2 用「父级与根同名 key」钉死根对象语义

`TestObservableDynamicIndexRoot`（`observable_trace_test.go:148`）让根上 `idx=2`、而 probe selector 自己的 data 里也放 `idx=0`，数组为 `[3,7,9]`。若动态下标误用当前段 `v2` 求值，会在 child 上取到 0 → 结果 3；实际实现固定传根 `v`（`evaluable.go:124`），取到根的 2 → 结果 `9`（断言 `observable_trace_test.go:158-163`）。

### 6.3 不能用「一句反射」概括的分支：typed nil、越界、不可见

`reflectSelect`（`evaluable.go:158`）先用 `resolvePotentialPointer` 解引用（`evaluable.go:160`、`evaluable.go:208-211`），再按 Kind 分派：

- **typed nil 指针**：`var h *observableHidden = nil` 时，`FieldByName("X")` 对解引用后的零值无效，也找不到同名方法，`reflect.Struct` 分支返回 `ok=false`（`evaluable.go:194-205`），最终报 `unknown parameter 'X' on *...observableHidden`（`evaluable.go:150`）。断言见 `observable_trace_test.go:215-220`。
- **typed slice 越界**：`nums[5]`（`[]int{1,2}`），进入 `reflect.Slice` 分支（`evaluable.go:182`），边界条件 `i >= 0 && vv.Len() > i`（`evaluable.go:183`）为假，再无同名方法，返回 `ok=false` → `unknown parameter '5' on []int`（`observable_trace_test.go:223-228`）。注意它与 `[]interface{}` 分支（`evaluable.go:141-145`，越界不报错而是保留 slice）行为不同，因此样本刻意选用 typed slice 才能得到可区分的错误。
- **不可见字段**：不依赖反射去取未导出字段（那会在 `field.Interface()` 处 panic，见 `evaluable.go:197`），而是由自定义 selector 对 `"private"` 返回受控错误；该错误经 `evaluable.go:130-132` 包成 `failed to select 'private' on *...pathRecorder: invisible field "private"`（断言 `observable_trace_test.go:231-236`，selector 实现在 `observable_trace_test.go:50-52`）。

三条错误分别落在不同分支、携带不同类型名与文案，`TestObservableDistinctErrors`（`observable_trace_test.go:208`）逐一断言它们互不相同。

---

## 7. context 取消：在下一个可观测边界停止

gval 的核心算符与 selector 循环本身不主动轮询 `ctx.Done()`；context 取消的内建检查点是**已注册函数的调用边界**。`Function` 注册的普通 Go 函数经 `toFunc` 包装（`functions.go:11`）：函数体在 goroutine 内执行（`functions.go:16-25`），外层用 `select` 在 `ctx.Done()` 与结果 channel 之间等待（`functions.go:27-33`；反射调用版本在 `functions.go:83-89`）。context 一旦取消，立即返回 `ctx.Err()`（`functions.go:28-29`），函数体即便已经在 goroutine 中开始，也不会再被等待，错误经 `callFunc`（`evaluable.go:227`）向上传播，最终由 `Language.EvaluateWithContext` 包成 `can not evaluate ...: context canceled`（`language.go:95`）。

两个样本覆盖两种时序（`TestObservableContextCancellation`，`observable_trace_test.go:244`）：

- **先取消**（`observable_trace_test.go:251`）：对 `gate() || late` 传入已 cancel 的 context。参数为零，`callFunc` 对零参数函数的求值循环（`evaluable.go:229-236`）不触碰任何 selector，随即在 `functions.go:27-29` 命中 `ctx.Done()`，返回 `context.Canceled`；`gate` 本体不执行（`gateCalls==0`），`||` 右侧 `late` 更不会执行，selector 记录为空（`observable_trace_test.go:260-264`）。
- **中途取消**（`observable_trace_test.go:268`）：表达式 `step && gate() && after`。`step` 是自定义 selector 的 key，`SelectGVal` 在记录该次访问后调用 `cancel()`（`observable_trace_test.go:47-49`），因此 `step` 这次访问被记录时 context 仍正常（`observable_trace_test.go:286-287`）；随后 `&&` 不短路（`step=true`），求右操作数时进入 `gate()` 的函数边界，在 `functions.go:27-29` 立即收到 `context.Canceled`，`gate` 本体零次执行、`after` 零次访问（断言 `observable_trace_test.go:280-287`）。可见取消不会打断已经在进行的一次 selector 访问，而是在它之后的下一个可观测边界（这里是函数调用）停止——selector 同时把「访问当下的 `ctx.Err()`」记入轨迹（`observable_trace_test.go:43-46`），使边界两侧的 context 状态可被审查。

---

## 8. 样本如何防止三类故意篡改

审查者若故意替换下列条件之一，现有样本会失败（已在本地逐一验证）：

1. **优先级**：把 `Precedence("+", 120)`（`gval.go:326`）调高到大于 `*`，`2*3+4` 的结合方式改变，折叠结果不再为 10，主表达式最终值改变，`TestObservableMasterTrace`（`observable_trace_test.go:93`）与示例输出 `Example_observableTrace`（`observable_trace_test.go:78`）同时失败。
2. **短路**：把 `&&` 的 `a == false` 改成 `a == true`（`gval.go:263`），`false && ...` 会错误地求值右侧，`TestObservableShortCircuit/and_false`（子用例 `observable_trace_test.go:122`）观察到本不该发生的 `[secret]` 访问而失败；主表达式轨迹也会改变（`observable_trace_test.go:102-105`）。
3. **selector 条件**：把 `variable` 中动态段的 `p.EvalString(c, v)`（`evaluable.go:124`）误用成当前段 `v2`，`TestObservableDynamicIndexRoot`（`observable_trace_test.go:148`）会取到 child 上的同名 `idx=0`，结果从 9 退化为 3（或因根/子结构不同而取空），断言失败。

此外 `TestObservableDistinctErrors`（`observable_trace_test.go:208`）把 typed-nil、typed-slice 越界、不可见字段三个分支的具体文案钉死，任何把这些分支合并成「统一反射/统一错误」的改动也会被捕获。

---

## 9. 复现

- 仅构建：`go build ./...`
- 完整验收：`go test ./... -count=1`
- 只看可观测示例：`go test -run Example_observableTrace -v`
- 只看本分析的样本：`go test -run TestObservable -v`

代码改动仅新增 `observable_trace_test.go`（记录型 selector、一个 `Example` 与多组 `Test`）与本文档；未改动 parser、operator、selector 的任何公共语义。
