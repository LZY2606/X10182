# gval 表达式全链路分析

本文跟踪**同一条**表达式从 `NewEvaluable` 解析到运行期求值的完整过程。所有行号均对应当前 commit `facecfb`（`git rev-parse HEAD` 为 `facecfbc424e9a871ceac184caab61173aae24a9`）。

跟踪表达式（常量、示例与测试见 `analysis_test.go`）：

```
1 + 2 * 3 < 100 && flag && root.ok || root.items[root.kidx] == 42
```

它同时包含任务要求的全部要素：

| 要素 | 表达式中的位置 |
| --- | --- |
| 伪装 token（camouflage） | 标识符尾部把 `&`、`|` 伪装；`[` 内子表达式把 `]` 伪装；结尾把 EOF 伪装 |
| 不同优先级二元运算 | `*`(150)、`+`(120)、`<`(40)、`==`(40)、`&&`(21)、`||`(20) |
| 短路布尔 | `&&` 与 `||` |
| 动态下标 | `root.items[root.kidx]`，下标本身是变量路径 |
| 自定义 selector | 根参数中的 `root` 是记录型 `gval.Selector`（`analysis_test.go:15` 的 `analysisNode`） |

可观测示例是 `Example_recordingSelector`（`analysis_test.go:304`）；记录型 selector `analysisNode.SelectGVal` 在 `analysis_test.go:28`，它把每次 `路径 + context 状态` 追加到日志。

## 1. 入口：从 NewEvaluable 到 parser

1. `Language.NewEvaluable` 用 `context.Background()` 转调 `NewEvaluableWithContext`（`language.go:62-64`）。
2. `NewEvaluableWithContext` 用 `newParser(expression, l)` 构造 parser（`language.go:67-69`）。`newParser` 初始化底层 `text/scanner`、文件名设为 `expression + "\t"`（用于报错定位），并设置标识符规则：首字符为字母/下划线，后续字符可为数字（`parser.go:20-28`、`parser.go:33-35`）。
3. 随后调用 `p.parse(c)`（`language.go:70`）。没有自定义 `Init` 扩展时直接进入 `ParseExpression`（`parse.go:80-86`）。
4. 解析返回后有一次**伪装结算**：若 parser 仍处于 camouflage 状态且最后一个 token 不是 EOF，则把当时记录的 `camouflage` 错误升级为解析错误（`language.go:71-73`）。也就是说，结尾那次“伪装 EOF”是合法终止，而“伪装到非 EOF token”会报错；错误再经 `language.go:75-76` 包装成带行列号的 `parsing error`。

`ParseExpression` 是一个单调栈主循环（`parse.go:24-40`）：每轮先用 `ParseNextExpression` 读出一个操作数（`parse.go:26`），再用 `parseOperator` 决定后面跟的是哪个算符（`parse.go:31`），最后把得到的 `stage` 压入 `stageStack`（`parse.go:33`）。当栈顶 stage 的 `infixBuilder == nil`（没有待结合的算符）时，弹出最终 Evaluable（`parse.go:37-39`）。

## 2. 伪装（camouflage）如何判断与“进位”

伪装机制本质是**只读不消费的回退一拍**，字段在 `parser.go:15-17`：`lastScan rune` 保存最近一次扫描结果，`camouflage error` 标记回退状态。

- `Scan()`：若处于伪装状态（`isCamouflaged()`，`parser.go:73-75`），清空标记并**原样返回上一次的 `lastScan`**，不再碰底层 scanner（`parser.go:64-67`）；否则正常扫描并记录 `lastScan`（`parser.go:68-70`）。
- `Camouflage()`：记录一个“本应期待别的 rune”的错误（`parser.go:80-85`）。`Expected` 把 `lastScan` 包成 `unexpectedRune`（`parser.go:117-120`、`parser.go:128-147`）。它不能在已经伪装时再次调用（`parser.go:81-83`）。
- `Peek()` 与 `Next()` 明确禁止在伪装 parser 上调用（`parser.go:91-96`、`parser.go:103-109`）；`Next()` 会把 `camouflage` 置为哨兵 `errCamouflageAfterNext`（`parser.go:98`、`parser.go:107`），该哨兵不被 `isCamouflaged` 视为真（`parser.go:74`），表示“在伪装之后又前进过”，此后旧的伪装承诺作废。

在本表达式中，伪装发生在三个具体位置：

1. **标识符尾部**。`parseIdent` 的 alternative 不断尝试 `.`、`(`、`[`；当扫描到既不是这三者的 rune（例如 `flag` 后面的 `&`、`root` 后面的 `|`）时，调用 `p.Camouflage("variable", '.', '(', '[')` 并返回变量 Evaluable（`parse.go:213-216`）。被多扫的那个 `&`/`|` 被“按住”一拍，下一次外层 `parseOperator` 的 `p.Scan()`（`parse.go:127`）会原样取回。
2. **动态下标内外各一次**。`parseIdent` 遇到 `[` 时递归调用 `ParseExpression` 解析下标（`parse.go:202-206`），子表达式解析完 `root.kidx` 后扫描到 `]`，走 `parse.go:213-215` 把 `]` 伪装回退；随后 `parseIdent` 的 `p.Scan()`（`parse.go:207`）取回 `]` 并把下标 Evaluable 追加进路径（`parse.go:208-210`）。追加后循环再次 `Scan`，读到下标后面的 `=`，它不属于 `.`/`(`/`[`，于是第二次伪装（`parse.go:185`、`parse.go:213-215`），把 `=` 交还给外层 `parseOperator` 去识别 `==`。
3. **算符探测失败**。`parseOperator` 扫描到非符号、非 Ident 的 rune 时调用 `Camouflage("operator")` 返回无 builder 的终结 stage（`parse.go:138-141`）；符号算符尝试拼接失败且不“必须成算符”时也会伪装返回（`parse.go:169-172`）。

此外还有一个符号拼接子机制与伪装协作：遇到符号字符时，`parseOperator` 用 `Peek` 不断尝试把后续符号并入算符名，只有当某前缀确实是已注册算符前缀（`isOperatorPrefix`，`operator.go:59-66`）时才 `Next()` 真正消费，例如 `|` 会并成 `||`、`*` 不会并成 `**`（`parse.go:130-137`）。

表达式结尾：最后一轮 `parseOperator` 扫描到 EOF，走 `parse.go:138-140` 伪装 EOF；`ParseExpression` 以“栈顶无 builder”正常结束（`parse.go:37-39`）。因为 `lastScan == EOF`，第 1 节第 4 步的结算不会报错（`language.go:71`）。

## 3. 优先级：stageStack 何时压栈、何时出栈结合

优先级数值在 `base` 语言中声明（`gval.go:304-334`）：`??`=0、`||`=20、`&&`=21、比较类（`== != > >= < <= =~ !~ in`）=40、位运算=60、移位=90、`+ -`=120、`* / %`=150、`**`=200。`Precedence` 通过 `newLanguageOperator` 把一个裸 `operatorPrecedence` 注册进 operators 表（`language.go:254-257`、`language.go:265-270`），语言合并时由 `operatorPrecedence.merge` / `infix.merge` 取较大值并挂到具体算符上（`operator.go:313-324`、`operator.go:343-369`）。

核心不变量写在 `operator.go:19`：**栈中的 operatorPrecedence 单调不减**。`stageStack.push` 的逻辑（`operator.go:21-40`）：

- 只要栈非空且 `栈顶优先级 >= 待入栈优先级`，就弹出栈顶 `a`，用 `a.infixBuilder(a.Evaluable, b.Evaluable)` 把左右操作数结合成新 Evaluable（`operator.go:22-24`），并把结果写回 `b.Evaluable`（`operator.go:36`），继续循环；
- 当栈顶优先级严格更小（或栈空）时，才把 `b` 追加进栈（`operator.go:38`）。

注意 `>=` 而非 `>`：这让同级算符**左结合**。每个新操作数在 `ParseExpression` 中以“终结 stage”入栈（终结 stage 的优先级为零值 0），因此入栈瞬间会把所有挂起的算符一次性弹出结合——这也是主循环能在 EOF 轮收尾的原因（`parse.go:37-39` 配合 `operator.go:22`）。

每轮循环产生的 stage 把“刚读到的操作数”和“紧随其后的算符”打包在一起（`parse.go:26-35`、`parse.go:142-154`）。以常量前缀 `1 + 2 * 3 < 100` 为例逐轮跟踪（数字由 `parseNumber` 经 `p.Const` 转为常量，`parse.go:96-102`、`evaluable.go:76-85`）：

1. 操作数 `1` + 识别出的 `+`（prec 120）组成 stage，栈空直接压入。
2. 操作数 `2` + `*`（prec 150）组成 stage：压入前比较栈顶，120 >= 150 为假，于是 `*` 连同 `2` 直接压栈，先不结合。
3. 操作数 `3` + `<`（prec 40）组成 stage：压入前循环弹出——150 >= 40，弹出 `*` 结合 `2 * 3`；120 >= 40，再弹出 `+` 结合 `1 + (2*3)`；栈空后该 stage 压栈。
4. 操作数 `100` + EOF（终结 stage，prec 0，`parse.go:138-140`）：压入前 40 >= 0，弹出 `<` 结合成 `(1 + 2*3) < 100`。

布尔部分同理：`&&`(21) 先于 `||`(20) 入栈，后续 `||` 入栈前会弹出挂起的 `&&`（21 >= 20）；两个同级 `&&` 之间因 `>=` 左结合。最终 AST 分组为

```
(( (1 + 2*3) < 100 ) && flag && root.ok) || (root.items[root.kidx] == 42)
```

把 `gval.go:306-307` 的 20/21 对调后，分组会变成 `const && flag && (root.ok || (... == 42))`，验收测试 `TestAnalysis_ShortCircuitAndDynamicIndex`（`analysis_test.go:83`）的第一个用例即由 `true` 变为 `false`（该变异已实测捕获）。

## 4. 常量子树为何能在解析期提前求值（以及边界在哪）

结合两个操作数时，`push` 检查**左右两个 Evaluable 是否都是常量**（`operator.go:28`）。判定常量不是看值，而是看函数指针：`IsConst` 比较自身指针与 `constant(nil)` 的闭包指针（`evaluable.go:298-302`）；所有 `p.Const` 都返回同一个 `//go:noinline` 的 `constant` 闭包（`evaluable.go:80-85`），因此字面值、折叠结果才会被判为常量。

两边都是常量时，`push` 立即用 `eval(nil, nil)` 求一次值，成功则把结果替换为新的 `constant(v)`（`operator.go:29-34`）。常量数字先经 `parseNumber` 转成 `float64`（`parse.go:97-101`），所以 `1 + 2 * 3 < 100` 在解析期就被折叠为 `constant(true)`，运行期不再产生任何运算与选择动作。`TestAnalysis_ParseHasNoSelectorSideEffects`（`analysis_test.go:68`）解析整条表达式后断言记录日志为空，证明折叠阶段完全没有触达 selector。

**“常量折叠不会偷跑副作用”的准确边界**由代码直接决定：

- 函数调用走 `callFunc`/`callEvaluable`，返回的是新建闭包，**永远不是** `constant`（`evaluable.go:227-239`、`evaluable.go:241-295`），所以任何包含 `Function(...)` 的子树在 `operator.go:28` 都判定为非 const，不会被折叠；函数只在运行期执行一次。`TestAnalysis_ConstantFoldingBoundaries` 的第一个子测试（`analysis_test.go:155`）用计数器证明 `1 + 2 * 3 + bump(4)` 解析期调用 0 次、求值期调用 1 次、结果为 `11`。把 `operator.go:28` 的条件改成 `if true` 会在解析期以 `nil` context 执行函数闭包而崩溃（该变异已实测捕获）。
- 前缀算符有一处**有意为之的例外**：`PrefixOperator` 的包装在操作数是常量时，会在解析期主动求一次值并替换成 `p.Const(v)`（`language.go:201-207`）。因此用户自定义前缀作用于常量字面量时，确实会在 parse 期执行一次——这不是“偷跑”，而是该扩展显式的折叠约定。第二个子测试（`analysis_test.go:185`）用自定义前缀 `#` 固定这一行为：解析期 1 次、求值期 0 次。
- 正则算符 `=~`/`!~` 是另一种“只提前编译、不提前取值”的优化：仅当右操作数为常量时，解析期 `regexp.Compile` 右值，左操作数仍留到运行期（`evaluable.go:304-334`、`evaluable.go:336-366`）。

## 5. `&&` / `||` 如何在运行期阻止右侧 selector 被调用

`&&`、`||` 各自由“短路谓词 + bool 实现”两部分组成（`gval.go:263-266`）：

- `&&`：谓词 `func(a){ return false, a == false }`——左值为 `false` 时短路，直接返回 `false`；
- `||`：谓词 `func(a){ return true, a == true }`——左值为 `true` 时短路，直接返回 `true`。

注册时 `infix.initiate` 发现 `shortCircuit != nil`，生成的 builder **不是**“先算 a 再算 b”的普通版本，而是短路版本（`operator.go:89-122`）：先只求左操作数（`operator.go:108`），调用谓词 `shortF(a)`，若第二个返回值为 `ok` 就**立刻返回，右操作数 `b` 一次都不求值**（`operator.go:112-114`）；只有不短路时才求右侧（`operator.go:115-119`）。对比无短路算符，左右两侧都会被无条件求值（`operator.go:90-101`）。

变量路径的求值发生在 `variable(path)` 闭包里，路径每一段才会触发一次选择（`evaluable.go:120-156`）。因此“右侧 selector 不被调用”在物理上由 `operator.go:112-114` 保证：右侧 Evaluable 根本没有被 invoke，闭包内的 `SelectGVal` 自然不会执行。

短路谓词看到的是 `a interface{}`。`&&`/`||` 的非短路实现通过 `getBoolOpFunc` 包装，支持把 `0/非0` 数字、`"true"/"false"` 字符串、可解引用指针等转成 bool（`operator.go:181-200`、`operator.go:144-180`）；但**短路判定本身是 `a == false` / `a == true` 的严格相等**（`gval.go:263`、`gval.go:265`），不做转换——只有真正的布尔值才会短路。把谓词中的 `a == false` 改为 `a == true`（或对 `||` 反向修改）后，记录日志立即出现越界访问，`TestAnalysis_ShortCircuitAndDynamicIndex`（`analysis_test.go:124`、`analysis_test.go:134`）对路径序列的断言全部失败（两种变异均已实测捕获）。

## 6. 变量路径：根参数、静态段、动态段如何分别求值

`parseIdent` 负责把标识符及其后续访问拼成一条路径（`parse.go:177-220`）：

- 第一段是标识符本身，立即放入 `keys := []Evaluable{p.Const(token)}`（`parse.go:183`）。因此即使是首段，路径元素也是 Evaluable，只是它是静态常量。
- `.ident`：扫描字段名并追加常量段（`parse.go:187-195`）。
- `[expr]`：递归 `ParseExpression` 解析方括号中的**任意表达式**，成功匹配 `]` 后把该 Evaluable 作为动态段追加（`parse.go:202-212`）。
- `(args)`：把当前路径当函数值取出来再调用（`parse.go:196-201`、`evaluable.go:241-295`），本表达式不涉及。
- 结束时把所有段交给 `p.Var(keys...)`（`parse.go:214-215`）。语言未安装自定义 `VariableSelector` 时，`Var` 返回默认的 `variable(path)`（`evaluable.go:97-102`）。

运行期 `variable(path)` 的关键点（`evaluable.go:120-156`）：

1. `v2 := v`（`evaluable.go:122`）：游标初始化为**根参数**，即调用 `eval(c, parameter)` 传入的整个 parameter；外层 map 解引用与内层选择用的是同一次调用的同一个根。
2. 对路径的**每一段**，先执行 `p.EvalString(c, v)`（`evaluable.go:124`）。注意第二个实参传的是原始根 `v` 而不是当前游标 `v2`：**动态下标段是相对于根参数重新求值的**。这正是 `root.items[root.kidx]` 能工作的原因——下标段 `root.kidx` 不是在 `root.items` 这个切片上找 `root`，而是回到根参数 map 找到 `root`，再经 selector 取 `kidx`。常量段（首段 `root`、静态字段）由 `constant` 直接返回字面量（`evaluable.go:81-85`），`EvalString` 对字符串走 `evaluable.go:69-70`。
3. 拿到字符串 key 后按游标 `v2` 的**当前类型**分派（`evaluable.go:128-152`）：
   - `Selector`：调用 `o.SelectGVal(c, k)`，错误被包装成 `failed to select '<k>' on <T>`（`evaluable.go:129-134`）。
   - `map[interface{}]interface{}`：直接按 key 取，key 不存在得到 `nil` 而非错误（`evaluable.go:135-137`）。
   - `map[string]interface{}`：同上（`evaluable.go:138-140`）。
   - `[]interface{}`：只有 `Atoi` 成功、非负且小于长度时才索引（`evaluable.go:141-145`）。
   - 其它类型（含 map 解引用后的结果）走 `reflectSelect`（`evaluable.go:146-151`、`evaluable.go:158-206`）。
4. 每个段的结果都赋回 `v2`，于是 `root.items` 先经 Selector 得到切片，下一段 `root.kidx`（动态段）相对根求值得到下标，再用该下标在切片上游走；下标字符串化依赖 `EvalString`，对数字走 `fmt.Sprintf("%v")`（`evaluable.go:64-73`），所以 `1.0` 变成 `"1"` 后由 `strconv.Atoi`（`evaluable.go:142`）转回整数。

本表达式在“flag=true、ok=false”分支的访问顺序（由 `analysis_test.go:25-39` 记录）为 `root.ok → root.items → root.kidx`，与 AST 的求值顺序一致；`root.items[root.kidx]` 取到 `42`，证明动态下标确实以正确的根对象和正确的中间切片求值，而不是把下标误当成切片字段。

默认 reflect 分派的各分支不是“一句话反射”能概括的（`evaluable.go:158-206`）：

- **map**：把字符串 key 按 map 的真实 key 类型转换（仅 string/int，`evaluable.go:215-225`），查值；查不到再尝试同名绑定方法（`evaluable.go:163-181`）。
- **slice**：必须能转成非负且小于长度的整数，否则再尝试绑定方法（`evaluable.go:182-193`）。
- **struct**：先 `FieldByName`（含导出/未导出字段），再 `MethodByName`（`evaluable.go:194-204`）。
- 指针先经 `resolvePotentialPointer` 解引用一层（`evaluable.go:208-213`）；typed-nil 指针解引用得到零值 `Value`，Kind 为 `Invalid`，三个 case 都不匹配，最终返回 `ok=false`（`evaluable.go:205`）。

## 7. 三类失败：typed nil、越界、不可见字段得到不同结果

对应验收测试 `TestAnalysis_DistinctFailures`（`analysis_test.go:215`）。三种失败的传播路径互不相同：

1. **typed nil 指针**（`node.Pub`，`node` 为 `(*analysisPlain)(nil)`）：map 段 `node` 取出 typed-nil 指针后进入 `reflectSelect`，`resolvePotentialPointer` 解引用为无效 Value，没有任何 case 命中，返回 `false`（`evaluable.go:159-162`、`evaluable.go:208-213`、`evaluable.go:205`），由默认分支包装为
   `unknown parameter 'Pub' on *gval_test.analysisPlain`（`evaluable.go:148-150`）。注意它与 `node` 本身是无类型 `nil` 时的报错不同（后者是 `unknown parameter 'Pub' on <nil>`），因为 `fmt.Sprintf("%T", ...)` 保留了指针的静态类型。
2. **越界**（`node[5]`，`node` 为 `[]string{"a"}`）：typed slice 走 reflect 分支，下标越界条件失败后虽会尝试 `MethodByName("5")`，仍返回 `false`（`evaluable.go:182-193`、`evaluable.go:205`），错误为 `unknown parameter '5' on []string`（`evaluable.go:150`）。
   需要注意一个既有特例：`[]interface{}` 分支在越界/负下标时**不返回错误**，而是静默保留原切片（`evaluable.go:141-145` 的条件不满足时直接结束 switch，循环继续）。因此“越界得到错误”只对走 reflect 的 typed slice 成立，`[]interface{}` 的静默行为是当前 commit 的既定语义，未在本次改动中修改。
3. **不可见（未导出）字段**（`node.sec`）：`FieldByName("sec")` 对未导出字段也返回“有效”Value（`evaluable.go:195-197`），随即 `field.Interface()` 触发 reflect 运行时 panic：`reflect.Value.Interface: cannot return value obtained from unexported field or method`。`variable()` 没有 recover，panic 直接穿过 `Evaluable` 与 `EvaluateWithContext`（`language.go:93-97` 也不 recover）到达调用方；这与导出字段不存在时的 `unknown parameter` 错误、以及 typed nil 的错误都不同。测试以 `recover` 固定这一第三类失败（`analysis_test.go:241-253`）。

## 8. context 取消在下一个可观测边界停止

gval 的算符 builder（`operator.go:90-122`）与 `variable` 的 map/slice/reflect 分支都**不主动检查** context——它们是纯内存操作。取消点被刻意放在**用户代码边界**：

- **自定义 selector 边界**：`variable` 把同一个 `c` 透传给 `SelectGVal(c, k)`（`evaluable.go:130`）。契约要求 selector 自行响应 `ctx.Done()`。记录型 selector `analysisNode.SelectGVal` 在每次入口先观察 `ctx.Err()`（`analysis_test.go:28-33`）。取消后表达式 `root.a == 1` 的执行在**第一次** selector 调用处停止：日志恰好一条 `root.a / context canceled`，错误经 `evaluable.go:131-133` 包装为 `failed to select 'a' ... context canceled`，右侧常量比较与任何后续段都不再执行（测试 `analysis_test.go:261-278`）。
- **函数边界**：`toFunc` 把用户函数放进 goroutine，主流程 `select { case <-ctx.Done(): return nil, ctx.Err(); case err := <-errCh: ... }`（`functions.go:27-33` 与反射版 `functions.go:83-89`）；若函数首参为 `context.Context`，框架还会把当前 ctx 注入参数（`functions.go:97-104`）。因此 `wait()` 这类 context-aware 函数在取消后从边界返回 `context canceled`（测试 `analysis_test.go:280-299`）。
- **解析期**：parse 阶段的 ctx 只传给扩展入口与折叠调用（如 `parse.go:44-54`、`operator.go:29` 的 `nil`、`language.go:202`），默认折叠的常量子树不持有 ctx 也无外部调用，故解析本身没有可取消的用户边界。

## 9. 样本与验收方式

- 可观测实现与样本全部在新增文件 `analysis_test.go`：记录型 selector（`analysis_test.go:15-39`）、解析无副作用（`analysis_test.go:68`）、短路/动态下标路径矩阵（`analysis_test.go:83`）、折叠边界（`analysis_test.go:151`）、三类失败（`analysis_test.go:215`）、取消边界（`analysis_test.go:260`）、可运行示例（`analysis_test.go:304`）。
- 未修改任何包内生产代码，公共语义不变；仅新增该测试文件与本文档。
- 环境准备：`go build ./...`；完整验收：`go test ./... -count=1`。
- 变异自检（均已实测）：交换 `gval.go:306-307` 的 `||`/`&&` 优先级 → 路径矩阵值断言失败；反转 `gval.go:263` 或 `gval.go:265` 的短路谓词条件 → 记录到的访问序列立即越界；使 `evaluable.go:129` 的 Selector 分支失效 → selector 日志消失且类型比较报错；把 `operator.go:28` 的折叠条件改为恒真 → 解析期以 nil context 执行函数闭包而崩溃。
