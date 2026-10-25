#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""黄金测试集评测 runner（检索轴 + 生成轴 baseline）。

设计要点：
- ground truth 用 **page 级**（evidence 的 doc+page 命中 top-K 即算召回）。
  原因：chunk 级受解析器阅读顺序影响（G0 有 30 条 MISSING 属于交织/表格/坏文本层），
  page 级能稳定衡量"检索有没有把证据找回来"，chunk 级作为附加诊断指标单独输出。
- 检索接口 /search 是 SSE，需要解析 `event: result`；/ask 是普通 JSON（仅 admin）。
- 全部题目都做**全库检索**（不限定 document_ids），这才是真实检索难度。

用法:
  # 先设环境变量（或在 GoLand/终端 profile 里持久化）
  #   GOLDEN_USER_ID=...   GOLDEN_DSN="host=localhost ... password=... dbname=paper_rag_test"
  python -X utf8 run_eval.py                                    # 默认混合+跟随配置的 rerank
  python -X utf8 run_eval.py --no-ask                           # 只跑检索轴
  python -X utf8 run_eval.py --hybrid false --rerank false      # 消融 A：纯向量无精排
  python -X utf8 run_eval.py --hybrid true  --rerank true       # 消融 C：混合+精排
"""
import argparse
import json
import os
import re
import sys
import unicodedata
from pathlib import Path

import psycopg2
import requests

BASE = Path(__file__).resolve().parent
DATA_DIR = BASE.parent / "data"         # golden_queries.json / docs.json
RESULTS_DIR = BASE.parent / "results"   # 评测产物（gitignore）

# 敏感信息走环境变量，不进仓库：
#   GOLDEN_USER_ID  管理员 user_id（/ask 仅 admin 可调，须在后端 admin_users 名单内）
#   GOLDEN_DSN      测试库连接串，形如 host=... user=... password=... dbname=paper_rag_test
DEFAULT_USER = os.environ.get("GOLDEN_USER_ID", "")
DEFAULT_DSN = os.environ.get("GOLDEN_DSN", "host=localhost port=5432 user=postgres dbname=paper_rag_test")


def nz(s: str) -> str:
    """与 G0 一致的归一化：NFKC → 去空白 → 去拉丁连字符 → 统一波浪号/破折号。"""
    s = unicodedata.normalize("NFKC", s)
    s = re.sub(r"\s+", "", s)
    s = re.sub(r"(?<=[A-Za-z])-(?=[A-Za-z])", "", s)
    for a, b in [("‐", "-"), ("‑", "-"), ("–", "-"), ("—", "-"),
                 ("～", "~"), ("〜", "~"), ("\ufffd", "")]:
        s = s.replace(a, b)
    return s


def load_doc_map(dsn: str) -> dict:
    """docs.json 的 R 编号 → 数据库 documents.id（按 filename 匹配）。"""
    docs = json.loads((DATA_DIR / "docs.json").read_text(encoding="utf-8"))["docs"]
    conn = psycopg2.connect(dsn)
    try:
        cur = conn.cursor()
        cur.execute("SELECT id, filename, status FROM documents")
        by_name = {}
        for did, fn, status in cur.fetchall():
            by_name.setdefault(fn, []).append((did, status))
    finally:
        conn.close()

    out = {}
    for d in docs:
        hits = by_name.get(d["filename"], [])
        if not hits:
            print(f"[WARN] {d['doc_id']} 未入库: {d['filename']}", file=sys.stderr)
            continue
        if len(hits) > 1:
            print(f"[WARN] {d['doc_id']} 有 {len(hits)} 条同名记录，取第一条", file=sys.stderr)
        did, status = hits[0]
        out[d["doc_id"]] = {"id": did, "filename": d["filename"], "status": status}
    return out


def post_search(base_url: str, user_id: str, query: str, top_k: int, hybrid: bool = None,
                rerank: bool = None) -> dict:
    """调 /search（SSE），返回 result 事件的 SearchResponse。

    必须显式 UTF-8 解码并按 "\\n" 切分：
      1) requests 对 text/event-stream 无 charset 时按 latin-1 解码；
      2) str.splitlines() 还会在 NEL(0x85) 处断行，而 UTF-8 中文的续字节里就可能有 0x85，
         于是长 JSON 被从中间切断（实测报 "Unterminated string at char 250"）。
    /search 是有限流（result 后 handler 返回），整包读取再按空行切事件最稳。
    """
    body = {"query": query, "top_k": top_k}
    if hybrid is not None:
        body["hybrid"] = hybrid
    if rerank is not None:
        body["rerank"] = rerank
    r = requests.post(
        f"{base_url}/api/v1/search",
        json=body,
        headers={"X-User-ID": user_id, "Content-Type": "application/json"},
        timeout=180,
    )
    r.raise_for_status()
    text = r.content.decode("utf-8", errors="replace")
    for block in text.split("\n\n"):
        event, data = None, None
        for line in block.split("\n"):
            if line.startswith("event:"):
                event = line[6:].strip()
            elif line.startswith("data:"):
                data = line[5:].strip()
        if event == "result" and data:
            return json.loads(data)
        if event == "error" and data:
            raise RuntimeError(f"/search error: {data}")
    raise RuntimeError("/search 未返回 result 事件")


def post_ask(base_url: str, user_id: str, query: str, hybrid: bool = None,
             rerank: bool = None) -> dict:
    body = {"query": query}
    if hybrid is not None:
        body["hybrid"] = hybrid
    if rerank is not None:
        body["rerank"] = rerank
    r = requests.post(
        f"{base_url}/api/v1/ask",
        json=body,
        headers={"X-User-ID": user_id, "Content-Type": "application/json"},
        timeout=300,
    )
    r.raise_for_status()
    return r.json()["answer"]


# 拒答判定：/ask 的 answer 出现这些特征即视为正确拒答（对应后端 no_context_reply 语义）
REFUSE_MARKERS = [
    "没有找到", "未找到", "无法回答", "找不到", "抱歉",
    "没有相关", "未收录", "没有检索到", "不存在于", "not found",
]


def is_refused(answer: str) -> bool:
    return any(m in answer for m in REFUSE_MARKERS)


def eval_query(q: dict, doc_map: dict, base_url: str, user_id: str, top_k: int, ask: bool,
               hybrid: bool = None, rerank: bool = None) -> dict:
    ev = q["evidence"]
    essential = [e for e in ev if e.get("role") == "essential"]
    supporting = [e for e in ev if e.get("role") != "essential"]
    behavior = q.get("expected_behavior", "answer")

    def gold_key(e):
        d = doc_map.get(e["doc"])
        return (d["id"], e["page"]) if d else None

    res = {"id": q["id"], "question": q["question"], "top_k": top_k,
           "hybrid": hybrid if hybrid is not None else "default(true)",
           "rerank": rerank if rerank is not None else "default(config)",
           "behavior": behavior}
    resp = post_search(base_url, user_id, q["question"], top_k, hybrid=hybrid, rerank=rerank)
    results = resp["results"]
    ranks = [(i + 1, r) for i, r in enumerate(results)]

    # --- 检索：page 级 ---
    hit_essential, miss_essential = [], []
    for e in essential:
        key = gold_key(e)
        rank = next((rk for rk, r in ranks if key and (r["document_id"], r["page"]) == key), None)
        (hit_essential if rank else miss_essential).append({"fact_id": e["fact_id"], "rank": rank})
    # --- 检索：严格 chunk 级（quote 出现在命中 chunk 文本中）---
    chunk_hits = []
    for e in essential:
        nq = nz(e["quote"])
        rank = next((rk for rk, r in ranks if nq and nq in nz(r["content"])), None)
        chunk_hits.append({"fact_id": e["fact_id"], "rank": rank})

    ranks_hit = [h["rank"] for h in hit_essential if h["rank"]]
    res["retrieval"] = {
        "page_recall": len(hit_essential) / len(essential) if essential else 0.0,
        "page_mrr": (1.0 / min(ranks_hit)) if ranks_hit else 0.0,
        "essential": hit_essential + miss_essential,
        "supporting_page_recall": (
            sum(1 for e in supporting
                if any((r["document_id"], r["page"]) == gold_key(e) for _, r in ranks)) / len(supporting)
            if supporting else None
        ),
        "chunk_recall": sum(1 for h in chunk_hits if h["rank"]) / len(essential) if essential else 0.0,
        "chunk_detail": chunk_hits,
        "top_k_items": [
            {"rank": rk, "doc": r["filename"][:36], "page": r["page"], "score": round(r["score"], 4)}
            for rk, r in ranks
        ],
    }

    # --- 生成：/ask ---
    if ask:
        a = post_ask(base_url, user_id, q["question"], hybrid=hybrid, rerank=rerank)
        cite_keys = {(c["document_id"], c["page"]) for c in a.get("citations", [])}
        ans_text = a.get("answer", "")
        res["answer"] = {
            "text": ans_text,
            "citation_page_recall": (sum(1 for e in essential if gold_key(e) in cite_keys) / len(essential)
                                      if essential else 0.0),
            "refused": is_refused(ans_text),
            "citations": [
                {"filename": c["filename"][:36], "page": c["page"], "score": round(c.get("score", 0), 4)}
                for c in a.get("citations", [])
            ],
            "latency_ms": a.get("latency_ms"),
        }
    return res


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base-url", default="http://localhost:8081")
    ap.add_argument("--user-id", default=DEFAULT_USER)
    ap.add_argument("--dsn", default=DEFAULT_DSN)
    ap.add_argument("--top-k", type=int, default=10)
    ap.add_argument("--no-ask", action="store_true", help="只跑检索轴")
    ap.add_argument("--hybrid", choices=["true", "false"], default=None,
                    help="请求级检索开关：true=混合，false=纯向量；不传走服务端默认（混合）")
    ap.add_argument("--rerank", choices=["true", "false"], default=None,
                    help="请求级精排开关：true=强制开，false=强制关；不传跟随服务端配置")
    ap.add_argument("--out", default=None, help="结果文件名（写入 results/），默认 eval_<时间戳>.json")
    args = ap.parse_args()

    if not args.user_id:
        sys.exit("缺少 user_id：设环境变量 GOLDEN_USER_ID 或传 --user-id（须为后端 admin_users 成员）")
    out_name = args.out or f"eval_{__import__('datetime').datetime.now():%Y%m%d_%H%M%S}.json"
    RESULTS_DIR.mkdir(exist_ok=True)

    hybrid = None
    if args.hybrid == "true":
        hybrid = True
    elif args.hybrid == "false":
        hybrid = False
    rerank = None
    if args.rerank == "true":
        rerank = True
    elif args.rerank == "false":
        rerank = False

    queries = json.loads((DATA_DIR / "golden_queries.json").read_text(encoding="utf-8"))["queries"]
    doc_map = load_doc_map(args.dsn)

    rows = []
    for q in queries:
        try:
            r = eval_query(q, doc_map, args.base_url, args.user_id, args.top_k, not args.no_ask,
                           hybrid=hybrid, rerank=rerank)
        except Exception as e:
            print(f"[FAIL] {q['id']}: {e}", file=sys.stderr)
            continue
        rows.append(r)
        rt = r["retrieval"]
        line = (f"{r['id']}  pageR@{args.top_k}={rt['page_recall']:.2f} "
                f"MRR={rt['page_mrr']:.2f} chunkR@{args.top_k}={rt['chunk_recall']:.2f}")
        if "answer" in r:
            line += f" citeR={r['answer']['citation_page_recall']:.2f}"
        print(line)

    if not rows:
        sys.exit("没有成功的题目")

    # 检索/引用指标只统计 answer/attribute_conflict 类（refuse 类无证据、不计入分母）
    graded = [x for x in rows if x.get("behavior") != "refuse"]
    refused_rows = [x for x in rows if x.get("behavior") == "refuse"]
    n = len(graded)
    summary = {
        "base_url": args.base_url,
        "top_k": args.top_k,
        "hybrid": hybrid if hybrid is not None else "default(true)",
        "rerank": rerank if rerank is not None else "default(config)",
        "queries": len(rows),
        "graded_queries": n,
        "refusal_queries": len(refused_rows),
        f"page_recall@{args.top_k}": sum(x["retrieval"]["page_recall"] for x in graded) / n if n else None,
        "page_mrr": sum(x["retrieval"]["page_mrr"] for x in graded) / n if n else None,
        f"chunk_recall@{args.top_k}": sum(x["retrieval"]["chunk_recall"] for x in graded) / n if n else None,
    }
    if not args.no_ask:
        summary["citation_page_recall"] = (
            sum(x["answer"]["citation_page_recall"] for x in graded) / n if n else None)
        if refused_rows:
            summary["refusal_accuracy"] = (
                sum(1 for x in refused_rows if x["answer"].get("refused")) / len(refused_rows))

    payload = {"summary": summary, "results": rows}
    out_path = RESULTS_DIR / out_name
    out_path.write_text(json.dumps(payload, ensure_ascii=False, indent=2), encoding="utf-8")

    print("\n=== SUMMARY ===")
    for k, v in summary.items():
        print(f"{k}: {v:.4f}" if isinstance(v, float) else f"{k}: {v}")
    print(f"\n明细: {out_path}")


if __name__ == "__main__":
    main()
