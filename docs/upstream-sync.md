# 上游代码同步（internal/upstream）

`internal/upstream/` 是 `generation_server` 依赖闭包的非测试源码副本，用于让本仓库脱离 `fengshen_slicer` 独立构建。

## 来源

- 仓库：`fengshen_slicer`
- 提交：`3572089`（2026-11-08，迁移基线）及其后的模型中心在途改动
- 包清单（以 `go list -deps` 实测闭包为准）：
  - `generation_server/facehaar`
  - `generation_server/highlight/audio`
  - `generation_server/highlight/domain`
  - `generation_server/highlight/media`
  - `generation_server/highlight/scoring`
  - `generation_server/highlight/subtitle`
  - `generation_server/highlight/transcript`
  - `generation_server/highlight/workspace`

## 复制规则

1. 只复制非 `_test.go` 源码；上游测试会额外引入 `highlight/analysis` 等测试专用依赖，不纳入本仓库。
2. 不复制上游 `testdata/`。
3. 导入路径改写：
   - `fengshen-slicer/generation_server/facehaar` → `github.com/aura-bootstrap/fengshen_model_center/internal/upstream/facehaar`
   - `fengshen-slicer/generation_server/highlight/<pkg>` → `github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/<pkg>`
4. 闭包内不得引入第三方模块依赖；若上游新增依赖，需同步更新 `go.mod` 并在此记录。

## 再同步步骤

```powershell
# 1. 在 fengshen_slicer 中记录上游提交
git -C <fengshen_slicer> rev-parse HEAD

# 2. 覆盖复制闭包（示例）
Copy-Item <fengshen_slicer>/src/generation_server/facehaar/* internal/upstream/facehaar/ -Recurse -Force
Copy-Item <fengshen_slicer>/src/generation_server/highlight/audio/* internal/upstream/highlight/audio/ -Recurse -Force
# ...其余 7 个包同理

# 3. 删除上游测试与测试数据
Get-ChildItem internal/upstream -Recurse -Filter *_test.go | Remove-Item
Get-ChildItem internal/upstream -Recurse -Directory -Filter testdata | Remove-Item -Recurse

# 4. 改写导入并回归
go build ./...
go vet ./...
go test ./...
```

## 漂移责任

- 本仓库不自动跟随上游；同步由人工执行并记录提交号。
- `fengshen_slicer` 若修改了上述包的行为，需评估对本仓库的影响并同步。
- 后续可考虑改为带版本 tag 的模块依赖或子模块，替代复制方式。
