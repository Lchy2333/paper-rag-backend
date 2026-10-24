# 黄金测试集语料目录

PDF 语料**不入 git**（体积大 + 部分规范文档有版权限制），此目录内容仅存在于本地。

## 恢复语料

1. 语料清单与完整性哈希见 `../data/docs.json`（每篇含 `filename` + `sha256`）。
2. 把 PDF 文件放入本目录（文件名须与 `docs.json` 完全一致）。
3. 校验完整性：

```powershell
cd ../tools
python -X utf8 g0_check.py   # docs 校验段会逐篇核对 sha256 与页数
```

校验全部通过即可用于入库与评测；任何一篇 sha256 不匹配都说明文件被替换或损坏。
