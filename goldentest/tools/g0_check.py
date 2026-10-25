#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""黄金评测集 G0 数据门禁。

页级校验（默认，无需数据库）：
  python g0_check.py
追加 chunk 级校验（需要本地 PostgreSQL + 已入库语料）：
  python g0_check.py --db
  python g0_check.py --db --dsn "postgresql://postgres:pass@localhost:5432/paper_rag"

chunk 级三态：
  OK       quote 完整落在单个 chunk 内
  SPLIT    quote 跨相邻 chunk（靠 overlap 可拼接），允许，但计入报告
  MISSING  quote 在库内找不到 -> 门禁失败

退出码：0 全部通过；1 存在失败项。
"""

import argparse
import hashlib
import json
import os
import re
import sys
import unicodedata
from pathlib import Path

BASE = Path(__file__).resolve().parent
DATA_DIR = BASE.parent / "data"      # docs.json / facts.json / golden_queries.json
CORPUS_DIR = BASE.parent / "corpus"  # PDF 语料（gitignore，本地放置）

R_ENUM = {"R1", "R2", "R3", "R4"}
A_ENUM = {"A1", "A2", "A3", "A4"}
BEHAVIOR_ENUM = {"answer", "refuse", "clarify", "attribute_conflict"}
ANSWER_TYPE_ENUM = {"numeric", "text", "set", "composite", "refusal", "clarification"}
STATUS_ENUM = {"draft", "verified"}


def nz(s: str) -> str:
    """四步归一化，缺一步都会掉命中：NFKC -> 去空白 -> 去拉丁连字符 -> 统一波浪号/破折号。"""
    s = unicodedata.normalize("NFKC", s)
    s = re.sub(r"\s+", "", s)
    s = re.sub(r"(?<=[A-Za-z])-(?=[A-Za-z])", "", s)
    s = s.replace("\u2010", "-").replace("\u2011", "-").replace("\u2013", "-").replace("\u2014", "-")
    s = s.replace("\uff5e", "~").replace("\u301c", "~")
    return s


def load(name):
    p = DATA_DIR / name
    if not p.exists():
        fail(f"缺少文件 {p}")
    return json.loads(p.read_text(encoding="utf-8"))


class Report:
    def __init__(self):
        self.errors = []
        self.warns = []

    def err(self, where, msg):
        self.errors.append(f"{where}: {msg}")

    def warn(self, where, msg):
        self.warns.append(f"{where}: {msg}")


def fail(msg):
    print(f"[G0] 无法启动: {msg}")
    sys.exit(2)


def check_docs(docs, rep):
    ids = set()
    for d in docs:
        did = d.get("doc_id", "<missing>")
        for key in ["doc_id", "filename", "title", "genre", "language", "pages", "status"]:
            if not d.get(key):
                rep.err(f"docs/{did}", f"缺少字段 {key}")
        if did in ids:
            rep.err(f"docs/{did}", "doc_id 重复")
        ids.add(did)

        path = CORPUS_DIR / d["filename"]
        if not path.exists():
            rep.err(f"docs/{did}", f"PDF 不存在: corpus/{d['filename']}")
            continue

        sha = hashlib.sha256(path.read_bytes()).hexdigest()
        if d.get("sha256") and d["sha256"] != sha:
            rep.err(f"docs/{did}", f"sha256 不匹配，期望 {d['sha256'][:12]}… 实际 {sha[:12]}…")

        try:
            import pymupdf
        except ImportError:
            fail("未安装 pymupdf，无法做页级 quote 校验（pip install pymupdf）")

        doc = pymupdf.open(str(path))
        if doc.page_count != d.get("pages"):
            rep.warn(f"docs/{did}", f"pages 字段 {d.get('pages')} 与实际 {doc.page_count} 不一致")
        doc.close()

    return ids


def check_facts(facts, docs_by_id, rep):
    """页级校验：quote 必须在指定 doc 的指定页内存在。"""
    import pymupdf

    cache = {}
    total = ok = 0
    for fid, f in facts.items():
        total += 1
        where = f"facts/{fid}"
        for key in ["type", "claim", "doc", "page", "section", "quote"]:
            if not f.get(key):
                rep.err(where, f"缺少字段 {key}")
        if f.get("type") not in {"verbatim", "derived"}:
            rep.err(where, f"type 非法: {f.get('type')}")

        d = docs_by_id.get(f.get("doc"))
        if not d:
            rep.err(where, f"doc 不存在: {f.get('doc')}")
            continue
        if d.get("status") == "needs_ocr":
            rep.err(where, f"doc {d['doc_id']} 状态为 needs_ocr，不应有 fact")

        page = f.get("page")
        if not isinstance(page, int) or not (1 <= page <= d["pages"]):
            rep.err(where, f"page 越界: {page}")
            continue
        pr = d.get("page_range")
        if pr and not (pr[0] <= page <= pr[1]):
            rep.err(where, f"page {page} 不在语料 page_range {pr} 内")

        q = nz(f["quote"])
        if len(q) < 10:
            rep.err(where, f"quote 过短（归一化后 {len(q)} 字符）")

        if d["doc_id"] not in cache:
            doc = pymupdf.open(str(CORPUS_DIR / d["filename"]))
            cache[d["doc_id"]] = [nz(doc[i].get_text("text")) for i in range(doc.page_count)]
            doc.close()

        text = cache[d["doc_id"]][page - 1]
        hits = text.count(q)
        if hits == 0:
            rep.err(where, f"quote 未在 {d['doc_id']} 第 {page} 页找到")
        elif hits > 1:
            rep.warn(where, f"quote 在第 {page} 页命中 {hits} 次，建议扩长或用 section 消歧")
        else:
            ok += 1

    return total, ok


def check_queries(queries, facts, rep):
    seen = set()
    ok = 0
    for q in queries:
        qid = q.get("id", "<missing>")
        where = f"queries/{qid}"
        before = len(rep.errors)
        if qid in seen:
            rep.err(where, "id 重复")
        seen.add(qid)

        r, a = q.get("retrieval_level"), q.get("answer_level")
        if r not in R_ENUM:
            rep.err(where, f"retrieval_level 非法: {r}")
        if a not in A_ENUM:
            rep.err(where, f"answer_level 非法: {a}")
        m = re.fullmatch(r"Q-(R[1-4])-(A[1-4])-\d{2}", qid or "")
        if not m:
            rep.err(where, "id 不符合 Q-R{n}-A{n}-{序号}")
        elif m.group(1) != r or m.group(2) != a:
            rep.err(where, "id 中的 R/A 等级与字段不一致")

        if not q.get("question"):
            rep.err(where, "question 为空")
        if q.get("expected_behavior") not in BEHAVIOR_ENUM:
            rep.err(where, f"expected_behavior 非法: {q.get('expected_behavior')}")
        if q.get("status") not in STATUS_ENUM:
            rep.err(where, f"status 非法: {q.get('status')}")

        ev = q.get("evidence", [])
        ess = [e for e in ev if e.get("role") == "essential"]
        for e in ev:
            fid = e.get("fact_id")
            f = facts.get(fid)
            if not f:
                rep.err(where, f"evidence 引用不存在的 fact: {fid}")
                continue
            if e.get("role") not in {"essential", "supporting"}:
                rep.err(where, f"{fid} role 非法: {e.get('role')}")
            if e.get("doc") and e["doc"] != f["doc"]:
                rep.err(where, f"{fid} doc 与 facts.json 不一致")
            if e.get("page") and e["page"] != f["page"]:
                rep.err(where, f"{fid} page 与 facts.json 不一致")
            if e.get("quote") and nz(e["quote"]) != nz(f["quote"]):
                rep.err(where, f"{fid} quote 与 facts.json 不一致")

        # refuse 类为"语料外负样本"，不要求 evidence；其余类按 R 档校验证据数
        if q.get("expected_behavior") != "refuse":
            if r in {"R1", "R2"} and len(ess) != 1:
                rep.err(where, f"{r} 应只有 1 条 essential，实际 {len(ess)}")
            if r in {"R3", "R4"} and len(ess) < 2:
                rep.err(where, f"{r} 应至少有 2 条 essential，实际 {len(ess)}")
            if r == "R4":
                if len({e.get("doc") for e in ess}) < 2:
                    rep.err(where, "R4 的 essential 证据必须跨文档")
            if r == "R3":
                if len({e.get("doc") for e in ess}) != 1:
                    rep.err(where, "R3 的 essential 证据必须同一文档")

        must = q.get("must_cite", [])
        ev_ids = {e.get("fact_id") for e in ev}
        for m in must:
            if m not in ev_ids:
                rep.err(where, f"must_cite 含未在 evidence 中的 {m}")
        if q.get("expected_behavior") == "answer" and not must:
            rep.err(where, "answer 类题目 must_cite 不应为空")

        ans = q.get("answer") or {}
        at = ans.get("type")
        if at not in ANSWER_TYPE_ENUM:
            rep.err(where, f"answer.type 非法: {at}")
        if at == "numeric":
            if ans.get("unit") is None:
                rep.err(where, "numeric 答案缺 unit")
            if not ans.get("tolerance"):
                rep.err(where, "numeric 答案缺 tolerance")
        if at == "composite" and not isinstance(ans.get("value"), dict):
            rep.err(where, "composite 答案的 value 应为对象")
        if not ans.get("keypoints") and at not in {"refusal", "clarification"}:
            rep.err(where, "答案缺 keypoints")

        fb = q.get("forbidden_claims", [])
        if q.get("expected_behavior") != "refuse" and not fb:
            rep.err(where, "forbidden_claims 不应为空")
        rb = q.get("judge_rubric", [])
        if not (2 <= len(rb) <= 5):
            rep.err(where, f"judge_rubric 应为 2-5 条，实际 {len(rb)}")

        if len(rep.errors) == before:
            ok += 1

    return len(queries), ok


def check_chunks(facts, docs_by_id, rep, dsn):
    try:
        import psycopg2
    except ImportError:
        fail("chunk 级校验需要 psycopg2（pip install psycopg2-binary）")

    dsn = dsn or os.environ.get("G0_DSN")
    if not dsn:
        pwd = os.environ.get("POSTGRES_PASSWORD", "")
        dsn = f"postgresql://postgres:{pwd}@localhost:5432/paper_rag"
    try:
        conn = psycopg2.connect(dsn)
    except psycopg2.OperationalError as e:
        fail(f"连接数据库失败，请用 --dsn 传入带密码的连接串。原始错误: {e}")

    def stitch(a, b):
        for l in range(min(len(a), len(b)), 0, -1):
            if a.endswith(b[:l]):
                return a + b[l:]
        return a + b

    stats = {"OK": 0, "SPLIT": 0, "MISSING": 0}
    details = []
    with conn.cursor() as cur:
        for did, d in docs_by_id.items():
            cur.execute(
                "SELECT id FROM documents WHERE filename = %s ORDER BY created_at DESC LIMIT 1",
                (d["filename"],),
            )
            row = cur.fetchone()
            if not row:
                for fid, f in facts.items():
                    if f.get("doc") == did:
                        stats["MISSING"] += 1
                        details.append((fid, "MISSING", "文档未入库"))
                continue
            doc_pk = row[0]

            cur.execute(
                "SELECT page, idx, content FROM chunks WHERE document_id = %s ORDER BY page, idx",
                (doc_pk,),
            )
            by_page = {}
            for page, idx, content in cur.fetchall():
                by_page.setdefault(page, []).append(nz(content or ""))

            merged = {}
            for page, chunks in by_page.items():
                text = chunks[0]
                for c in chunks[1:]:
                    text = stitch(text, c)
                merged[page] = text

            for fid, f in facts.items():
                if f.get("doc") != did:
                    continue
                q = nz(f["quote"])
                page = f["page"]
                if q in merged.get(page, ""):
                    stats["OK"] += 1
                else:
                    nxt = merged.get(page + 1, "")
                    if q in stitch(merged.get(page, ""), nxt):
                        stats["SPLIT"] += 1
                        details.append((fid, "SPLIT", f"跨第 {page}/{page + 1} 页，应拆成两条 evidence"))
                    else:
                        stats["MISSING"] += 1
                        details.append((fid, "MISSING", f"{did} 第 {page} 页 chunk 内未找到"))

    conn.close()
    for fid, st, msg in details:
        if st == "MISSING":
            rep.err(f"chunks/{fid}", msg)
        else:
            rep.warn(f"chunks/{fid}", msg)
    return stats


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--db", action="store_true", help="追加 chunk 级校验")
    ap.add_argument("--dsn", default=None, help="PostgreSQL DSN")
    ap.add_argument("--strict", action="store_true", help="quote 页内多命中视为失败")
    args = ap.parse_args()

    data = load("docs.json")
    docs = data.get("docs", [])
    facts = load("facts.json").get("facts", {})
    queries = load("golden_queries.json").get("queries", [])
    docs_by_id = {d["doc_id"]: d for d in docs if d.get("doc_id")}

    rep = Report()
    check_docs(docs, rep)
    total_f, ok_f = check_facts(facts, docs_by_id, rep)
    total_q, ok_q = check_queries(queries, facts, rep)

    if args.strict:
        for w in list(rep.warns):
            if "命中" in w and "次" in w:
                rep.warns.remove(w)
                rep.errors.append(w)

    print("=" * 58)
    print("G0 数据门禁")
    print("=" * 58)
    print(f"docs.json          {len(docs):>4} 篇")
    print(f"facts.json         {total_f:>4} 条   页级命中 {ok_f}")
    print(f"golden_queries     {total_q:>4} 道   结构通过 {ok_q}")

    if args.db:
        st = check_chunks(facts, docs_by_id, rep, args.dsn)
        print(f"chunk 级           OK {st['OK']} / SPLIT {st['SPLIT']} / MISSING {st['MISSING']}")

    if rep.warns:
        print(f"\n-- 警告 {len(rep.warns)} --")
        for w in rep.warns:
            print("  ! " + w)
    if rep.errors:
        print(f"\n-- 失败 {len(rep.errors)} --")
        for e in rep.errors:
            print("  x " + e)
        print("\n结果: FAIL")
        sys.exit(1)
    print("\n结果: PASS")


if __name__ == "__main__":
    main()
