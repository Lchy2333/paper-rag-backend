# -*- coding: utf-8 -*-
"""答案正确性判定（黄金测试集评测扩展）。

三通道路由（按题自动选择）：
  A numeric   数值程序判：正则抽取回答中的数字，与 value±tolerance 比对——
              确定性、零成本，凡能程序判的绝不走 LLM
  B semantic  LLM 裁判：异源模型读 keypoints + forbidden_claims + judge_rubric 打分
  C refusal   LLM 裁判拒答质量：refused（规范拒答）/ hallucinated（编造）/ evasive（含糊）

裁判模型必须与被测 chat 模型异源异构（避免自我偏好）：
被测 = 方舟 deepseek-v4-flash，裁判默认 SiliconFlow Qwen2.5-72B-Instruct。

标定：`python judge.py --calibrate`（GOLDEN_JUDGE_API_KEY 环境变量）
用"已知对/已知错"的小样本验证裁判本身可信后，才允许进正式评测。
"""
import argparse
import json
import os
import re
import sys

import requests

DEFAULT_BASE_URL = "https://api.siliconflow.cn/v1"
DEFAULT_MODEL = "Qwen/Qwen2.5-72B-Instruct"
TIMEOUT = 60

_NUM_RE = re.compile(r"-?\d+(?:\.\d+)?")


# ---------------------------------------------------------------- 通道 A：数值

def _numeric_leaves(obj, prefix=""):
    """递归取出 value 中全部数值叶子（bool 不算数），返回 [(路径, float)]。"""
    leaves = []
    if isinstance(obj, dict):
        for k, v in obj.items():
            leaves += _numeric_leaves(v, f"{prefix}{k}.")
    elif isinstance(obj, list):
        for i, v in enumerate(obj):
            leaves += _numeric_leaves(v, f"{prefix}{i}.")
    elif isinstance(obj, (int, float)) and not isinstance(obj, bool):
        leaves.append((prefix.rstrip("."), float(obj)))
    return leaves


def _scalar_leaves(obj):
    """递归计数全部标量叶子（用于判断 composite 是否纯数值）。"""
    if isinstance(obj, dict):
        return sum(_scalar_leaves(v) for v in obj.values())
    if isinstance(obj, list):
        return sum(_scalar_leaves(v) for v in obj)
    return 1


def _tolerance(expected: float, tol: dict) -> float:
    mode, v = tol.get("mode", "absolute"), float(tol.get("value", 0))
    return v if mode == "absolute" else abs(expected) * v  # relative: v 为比例（0.01=1%）


def route(q: dict) -> str:
    """题目 -> 判定通道。"""
    if q.get("expected_behavior") == "refuse":
        return "refusal"
    ans = q.get("answer") or {}
    if ans.get("type") == "numeric":
        return "numeric"
    if ans.get("type") == "composite":
        val = ans.get("value") or {}
        leaves = _numeric_leaves(val)
        if leaves and len(leaves) == _scalar_leaves(val):
            return "numeric"  # composite 但叶子全为数字 -> 程序判
    return "semantic"


def _cjk_fragments(path: str):
    """从键路径提取中文标签片段（"开裂荷载_kN"→["开裂荷载"]；无中文则空）。"""
    frs = []
    for seg in path.split("."):
        base = seg.split("_")[0]
        if re.search(r"[\u4e00-\u9fff]", base):
            frs.append(base)
    return frs


def _hit_near(text: str, num_positions, expected: float, tol: float, fragments) -> bool:
    """期望值命中判定：数字出现在值±tol 内，且（若有中文标签）须与标签邻近。

    邻近窗口 15 字符——防"数值互换"误判：互换后两个数字都在文中，
    纯全局命中测不出来；要求值贴着自己的标签出现，互换方会因超窗落空。
    无中文标签（单值题/英文键）时退回全局命中。
    """
    val_hits = [p for p, n in num_positions if abs(n - expected) <= tol]
    if not val_hits:
        return False
    frag_positions = [m.start() for f in fragments
                      for m in re.finditer(re.escape(f), text)]
    if not fragments or not frag_positions:
        return True
    return any(abs(vp - fp) <= 15 for vp in val_hits for fp in frag_positions)


def judge_numeric(q: dict, answer_text: str) -> dict:
    """通道 A：抽取回答中的数字与 value±tolerance 比对。

    多键 composite 用"标签邻近"配对（防互换误判 correct，互换降级 partial，
    严格的 wrong 交给 LLM 语义通道）；试件编号等杂散数字不产生误报。
    """
    ans = q.get("answer") or {}
    val = ans.get("value")
    if ans.get("type") == "composite":
        leaves = _numeric_leaves(val)
    else:
        leaves = [("value", float(val))]

    clean = answer_text.replace(",", "")
    num_positions = [(m.start(), float(m.group())) for m in _NUM_RE.finditer(clean)]
    tol = ans.get("tolerance") or {"mode": "absolute", "value": 0}

    per_key = {}
    for key, expected in leaves:
        ok = _hit_near(clean, num_positions, expected,
                       _tolerance(expected, tol), _cjk_fragments(key))
        per_key[key] = ok

    hits = sum(per_key.values())
    verdict = "correct" if hits == len(per_key) else ("partial" if hits else "wrong")

    # 单位检查：要求单位且回答未出现 -> correct 降为 partial
    unit_ok = None
    unit = ans.get("unit")
    if unit:
        unit_ok = unit in answer_text
        if verdict == "correct" and q.get("output_format", {}).get("unit_required") and not unit_ok:
            verdict = "partial"

    return {"lane": "numeric", "verdict": verdict,
            "per_key": per_key, "unit_ok": unit_ok,
            "keypoint_hits": list(per_key.values())}


# ------------------------------------------------------- LLM 裁判（通道 B/C）

def _chat(client: dict, system: str, user: str) -> str:
    resp = requests.post(
        f"{client['base_url']}/chat/completions",
        json={"model": client["model"], "temperature": 0, "max_tokens": 500,
              "messages": [{"role": "system", "content": system},
                           {"role": "user", "content": user}]},
        headers={"Authorization": f"Bearer {client['api_key']}"},
        timeout=TIMEOUT,
    )
    resp.raise_for_status()
    return resp.json()["choices"][0]["message"]["content"]


def _parse_json(text: str) -> dict:
    """剥代码围栏后取第一个完整 JSON 对象。"""
    m = re.search(r"\{.*\}", text, re.S)
    if not m:
        raise ValueError(f"无 JSON：{text[:120]}")
    return json.loads(m.group())


_JUDGE_SYSTEM = (
    "你是 RAG 问答评测的严格裁判。只依据给定的参考要点和判分规则判断回答正确性，"
    "禁止使用你自己的外部知识补充、纠正或宽容回答。只输出 JSON。"
)


def judge_semantic(q: dict, answer_text: str, client: dict) -> dict:
    """通道 B：语义题裁判（correct/partial/wrong + 逐要点命中）。"""
    ans = q.get("answer") or {}
    keypoints = ans.get("keypoints") or []
    forbidden = q.get("forbidden_claims") or []
    rubric = q.get("judge_rubric") or []
    extra = ""
    if q.get("expected_behavior") == "attribute_conflict":
        extra = "\n注意：本题考查冲突归因，回答必须并列呈现双方立场，只采用一方即 wrong。"

    user = f"""
    ## 问题
{q['question']}

## 待评回答
{answer_text}

## 参考要点（逐条判断回答是否覆盖，注意措辞可以不同但语义必须一致）
{chr(10).join(f"{i + 1}. {kp}" for i, kp in enumerate(keypoints))}

## 禁止出现的主张（回答中出现即违例）
{chr(10).join(forbidden) if forbidden else "（无）"}

## 判分规则
{chr(10).join(rubric)}{extra}

仅输出 JSON：
{{"verdict":"correct|partial|wrong","keypoint_hits":[与要点等长的布尔数组],"forbidden_violations":["命中禁答项的原句，无则空数组"],"reason":"一句话依据"}}"""

    data = _parse_json(_chat(client, _JUDGE_SYSTEM, user))
    verdict = str(data.get("verdict", "")).lower()
    if verdict not in {"correct", "partial", "wrong"}:
        raise ValueError(f"非法 verdict: {verdict}")
    hits = data.get("keypoint_hits") or []
    hits = (list(hits) + [False] * len(keypoints))[:len(keypoints)]  # 补齐/截断
    # 过滤空违例：裁判偶尔返回 [''] 这类空串数组（reason 往往自相矛盾地说"未违反"）
    violations = [str(v).strip() for v in (data.get("forbidden_violations") or [])]
    violations = [v for v in violations if v]
    return {"lane": "semantic", "verdict": verdict,
            "keypoint_hits": [bool(h) for h in hits],
            "forbidden_violations": violations,
            "reason": str(data.get("reason", ""))[:200]}


def judge_refusal(q: dict, answer_text: str, client: dict) -> dict:
    """通道 C：拒答题裁判（refused/hallucinated/evasive）。"""
    user = f"""## 问题（该问题的答案不在知识库语料中）
{q['question']}

## 系统回答
{answer_text}

判定系统行为属于哪类：
- "refused"：明确说明文档/语料中未找到相关信息或无法回答（含礼貌性抱歉）
- "hallucinated"：给出了具体的编造内容（数值、名称、结论等语料中不存在的"事实"）
- "evasive"：含糊其辞，既未明确拒答也未给出编造内容

仅输出 JSON：
{{"verdict":"refused|hallucinated|evasive","reason":"一句话依据"}}"""

    data = _parse_json(_chat(client, _JUDGE_SYSTEM, user))
    verdict = str(data.get("verdict", "")).lower()
    if verdict not in {"refused", "hallucinated", "evasive"}:
        raise ValueError(f"非法 verdict: {verdict}")
    return {"lane": "refusal", "verdict": verdict,
            "keypoint_hits": [], "forbidden_violations": [],
            "reason": str(data.get("reason", ""))[:200]}


def judge_answer(q: dict, answer_text: str, client: dict) -> dict:
    """统一入口：路由 -> 判定 -> 异常兜底（单题裁判失败不中断评测）。"""
    lane = route(q)
    try:
        if lane == "numeric":
            return judge_numeric(q, answer_text)
        if lane == "refusal":
            return judge_refusal(q, answer_text, client)
        return judge_semantic(q, answer_text, client)
    except Exception as e:
        return {"lane": lane, "verdict": "judge_error",
                "keypoint_hits": [], "forbidden_violations": [],
                "error": str(e)[:200]}


# ---------------------------------------------------------------- 标定

def _calibration_cases():
    """标定集：已知对/已知错的小样本，验证裁判本身可信。"""
    q_num = {"question": "CFT1 试件的开裂荷载和极限荷载分别是多少？",
             "expected_behavior": "answer",
             "forbidden_claims": ["开裂荷载为 10,151 kN", "极限荷载为 7,000 kN"],
             "judge_rubric": ["两个数值与单位都正确算 correct", "数值互换算 wrong"],
             "output_format": {"unit_required": False},
             "answer": {"type": "composite",
                        "value": {"开裂荷载_kN": 7000, "极限荷载_kN": 10151},
                        "tolerance": {"mode": "absolute", "value": 1},
                        "keypoints": ["开裂荷载 7,000 kN", "极限荷载 10,151 kN"]}}
    q_sem = {"question": "短柱和梁柱节点各自的破坏形式是什么？",
             "expected_behavior": "answer",
             "forbidden_claims": ["短柱发生核心区混凝土压溃破坏"],
             "judge_rubric": ["两种破坏形式都答对算 correct", "把两者说反算 wrong"],
             "output_format": {"unit_required": False},
             "answer": {"type": "composite",
                        "value": {"短柱": "剪切破坏", "节点": "核心区混凝土压溃破坏"},
                        "keypoints": ["短柱发生剪切破坏", "节点发生核心区混凝土压溃破坏"]}}
    q_ref = {"question": "Muttoni 教授的博士导师是哪位学者？",
             "expected_behavior": "refuse", "answer": {"type": "refusal"},
             "forbidden_claims": [], "judge_rubric": [], "output_format": {}}
    return [
        ("num-pos", q_num, "CFT1 的开裂荷载为 7,000 kN，极限荷载为 10,151 kN。", "correct"),
        # 互换场景：邻近匹配下一个键会捡到自己的值，程序判保守降级为 partial
        # （answer_accuracy 只认 correct，互换仍算不合格；严格 wrong 由 LLM 语义通道给）
        ("num-neg", q_num, "CFT1 的开裂荷载为 10,151 kN，极限荷载为 7,000 kN。", "partial"),
        ("num-miss", q_num, "CFT1 的开裂荷载为 7,000 kN。", "partial"),
        ("sem-pos", q_sem, "轴压短柱发生剪切破坏，节点试件发生核心区混凝土压溃破坏。", "correct"),
        ("sem-half", q_sem, "短柱发生剪切破坏。", "partial"),
        ("sem-neg", q_sem, "短柱发生核心区混凝土压溃破坏，节点发生剪切破坏。", "wrong"),
        ("ref-pos", q_ref, "抱歉，在已上传的文档中没有找到与您问题相关的内容。", "refused"),
        ("ref-neg", q_ref, "Muttoni 教授的博士导师是苏黎世联邦理工学院的 Bažant 教授。", "hallucinated"),
    ]


def calibrate(client: dict) -> int:
    cases = _calibration_cases()
    passed = 0
    for name, q, text, expect in cases:
        got = judge_answer(q, text, client)["verdict"]
        ok = got == expect
        passed += ok
        print(f"  {'PASS' if ok else 'FAIL'}  {name:9s} expect={expect:13s} got={got}")
    print(f"\n标定结果: {passed}/{len(cases)}")
    return 0 if passed == len(cases) else 1


if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument("--calibrate", action="store_true", help="运行裁判标定集")
    ap.add_argument("--base-url", default=DEFAULT_BASE_URL)
    ap.add_argument("--model", default=DEFAULT_MODEL)
    args = ap.parse_args()
    key = os.environ.get("GOLDEN_JUDGE_API_KEY", "")
    if not key:
        sys.exit("缺少环境变量 GOLDEN_JUDGE_API_KEY")
    sys.exit(calibrate({"base_url": args.base_url, "api_key": key, "model": args.model}))
