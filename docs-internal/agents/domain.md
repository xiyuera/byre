# Domain Docs

工程技能探索代码库时，如何消费本仓库的领域文档。

## 探索前先读这些

- 仓库根的 **`GLOSSARY.md`**（单上下文布局）。
- **`docs/adr/`**：读与本次改动区域相关的 ADR。

文件不存在就**安静继续**，不要提示缺失、不要主动建议先建；`domain-modeling` 技能会在术语或决策
真正确定时按需创建/更新它们。

## 文件结构（single-context）

```
/
├── GLOSSARY.md
├── docs/adr/
│   ├── 0001-....md
│   └── 0002-....md
```

## 使用 glossary 的词汇

输出里命名领域概念（issue 标题、重构提案、假设、测试名）时，用 `GLOSSARY.md` 里定义的词，
不要漂移到 glossary 明确回避的同义词。需要的概念还没进 glossary 时，这是信号：要么你在发明
项目不用的语言（重新考虑），要么是真缺口（记下来交给 `domain-modeling`）。

## ADR 冲突要显式指出

输出与已有 ADR 冲突时，明确说出来，不要静默覆盖：

> _与 ADR-0007（event-sourced orders）冲突，但值得重开，因为……_
