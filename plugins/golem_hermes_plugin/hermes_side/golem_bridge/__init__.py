"""golem_bridge —— hermes 侧微信能力桥。

把 golem_hermes_plugin（Go 插件）内嵌的工具桥暴露为 hermes agent 工具。
所有操作只作用于"当前会话"：Go 侧按活跃会话绑定校验，agent 无法指定任意 wxid。

配置（环境变量，写入各 profile 的 .env 即可）：
  GOLEM_BRIDGE_URL   默认 http://127.0.0.1:8790
  GOLEM_BRIDGE_TOKEN 必须与 golem 插件 config.toml 的 bridge_token 一致
"""

from __future__ import annotations

import json
import os
import urllib.error
import urllib.request

BRIDGE_URL = os.environ.get("GOLEM_BRIDGE_URL", "http://127.0.0.1:8790").rstrip("/")
BRIDGE_TOKEN = os.environ.get("GOLEM_BRIDGE_TOKEN", "")

EMOTION_HINT = "开心 | 笑哭 | 狗头 | 赞 | 疑惑 | 生气 | 伤心 | 无语 | 可爱 | 其他"

USAGE_GUIDELINES = (
    "微信工具使用准则（golem_bridge）：\n"
    "- 这些工具只作用于发起当前对话的微信聊天，你不能也没有必要指定任何 wxid。\n"
    "- 发表情包的铁律：用户要表情包（发个表情/来张狗头/整一个可爱的等）时，"
    "必须调用 send_emoji 工具，这是把表情发进微信的唯一途径。\n"
    "- 严禁用 generate_image / 画图 / 文件类工具自己造图片来冒充表情包；"
    "严禁在回复里出现任何文件路径（MEDIA:、C:\\ 开头等）——路径发不到微信，用户只会看到一串乱码。\n"
    "- send_emoji：在情绪合适时发一张对应情绪的表情包（可选情绪: " + EMOTION_HINT + "）。"
    "表情包是稀缺动作不是标配：大多数回复不带表情，一条回复最多调一次 send_emoji；"
    "返回「冷却中」就说明刚发过，绝不要再试。普通群聊里感觉对了才用。\n"
    "- 真人感铁律：回复正文里严禁任何舞台指示——括号动作（笑）（摊手）（狗头）、星号动作 *歪头*、"
    "旁白【沉默】都不许出现。想表达这类情绪时改调 send_emoji 发真表情：想（狗头）就 send_emoji(emotion=\"狗头\")。\n"
    "- send_image：回复内容较长（超过约300字）、含表格/列表/代码时，"
    "改用 markdown 渲染成图片发送，正文只留一两句话。\n"
    "- generate_image（画坊）：用户要求画图/生成图片/海报，或要把某人的照片改成某种风格时调用。"
    "它立刻返回工单号并在后台作画，画好会自动发到群里——你只需顺势接一句话，不要催、不要重试、不要再调其他工具。\n"
    "- 画坊铁律：只有【直接对你下达】的画图需求才开单——当前发言人艾特你或直接对你说。"
    "群里 @ 别人的画图请求、转述别人的约稿、聊天里提到想画什么，都不是叫你，绝不开单；"
    "表情包需求绝不开画坊（成本差一个量级）；一次回复最多开一单；"
    "返回产能售罄/未开张/不是对我的单就如实转告，不要反复尝试。\n"
    "- send_video：用户明确发来视频直链或明确要求发某个具体视频时调用。"
    "只发用户主动给的链接或群里上下文直接对应的视频；绝不主动搜寻、推荐或批量分发来路不明的视频源"
    "（版权与内容合规由链接提供方负责，微信对违规内容零容忍）；"
    "超过3MB/非直链/配额用尽时如实说明原因，不要找别的源硬凑。\n"
    "- reply_quote：需要精确回应某人某条消息时用引用回复。\n"
    "- get_member_profile / get_group_members / search_chat_history / get_recent_context："
    "需要了解群成员或翻聊天记录时使用。\n"
    "- 发画像的规范：用户要看某人的画像/人设卡时，直接调 send_member_profile_card(name)，"
    "它会把完整画像渲染成图（与监控台同源数据），正文只配一句俏皮话；"
    "查无此人就如实说该同学语料还不够。不要用 send_image 自己拼画像图。\n"
    "- 工具失败时直接用文字回复，不要重试超过一次。\n"
    "- remember：聊天中听到关于【当前发言人】的重要稳定事实（喜好、计划、承诺、身份、金句）"
    "时主动记住；只能记当前发言人自己的事，绝不能替别人记；手机号/住址等隐私一律不记。\n"
    "- 透明原则：任何人问「你记了我什么」「你了解我什么」时，如实完整地展示记得的内容，"
    "绝不隐瞒。\n"
    "- 会话隔离：所有工具严格绑定当前对话；多个会话同时运行时若无法确定目标会被拒绝，"
    "此时如实告知对方稍后再试即可，不要猜测。"
)


def _call(method: str, path: str, payload: dict | None = None, session_id: str = "",
          timeout: int = 120) -> dict:
    url = BRIDGE_URL + path
    data = None
    headers = {"Authorization": "Bearer " + BRIDGE_TOKEN}
    if session_id:
        headers["X-Hermes-Session-Id"] = session_id
    if payload is not None:
        data = json.dumps(payload, ensure_ascii=False).encode("utf-8")
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as e:
        try:
            body = json.loads(e.read().decode("utf-8"))
            return {"ok": False, "error": body.get("error", f"HTTP {e.code}")}
        except Exception:
            return {"ok": False, "error": f"HTTP {e.code}"}
    except Exception as e:  # 连接失败等
        return {"ok": False, "error": f"工具桥不可达: {e}"}


def _make_handler(method: str, path: str, timeout: int = 120):
    def handler(args: dict, **kwargs) -> str:
        result = _call(method, path, payload=args,
                       session_id=str(kwargs.get("session_id") or ""), timeout=timeout)
        return json.dumps(result, ensure_ascii=False)

    return handler


TOOLS = [
    (
        "send_emoji",
        {
            "type": "object",
            "properties": {
                "emotion": {"type": "string", "description": "情绪分类，可选值: " + EMOTION_HINT},
                "keyword": {"type": "string", "description": "可选关键词，用于在库里进一步筛选"},
            },
            "required": ["emotion"],
        },
        "在当前微信聊天发送一张按情绪挑选的表情包",
        _make_handler("POST", "/v1/emoji", timeout=30),
    ),
    (
        "send_image",
        {
            "type": "object",
            "properties": {
                "kind": {"type": "string", "enum": ["markdown", "text"],
                          "description": "markdown=完整渲染，text=纯文本成图"},
                "content": {"type": "string", "description": "要渲染的内容正文"},
            },
            "required": ["kind", "content"],
        },
        "在当前微信聊天把 markdown/文本渲染成图片并发送（长内容首选）",
        _make_handler("POST", "/v1/image", timeout=150),
    ),
    (
        "generate_image",
        {
            "type": "object",
            "properties": {
                "prompt": {"type": "string",
                            "description": "画面描述：具体、有画面感的一句话，可含主体/风格/氛围（如：一只戴着墨镜的橘猫在霓虹灯下吃烤串，赛博朋克风）"},
                "ref": {"type": "string",
                         "description": "改图素材（改图时必填）：\"@某人的昵称\"=用TA最近发的照片；\"quoted\"=当前被引用的那张图；\"latest\"=群里最近一张图。留空=凭空画"},
                "size": {"type": "string", "enum": ["auto", "1024x1024", "1536x1024", "1024x1536"],
                          "description": "画布尺寸，默认 auto"},
            },
            "required": ["prompt"],
        },
        "画坊：生成一张图片发到当前聊天（后台异步作画，画好自动发群）；传 ref 可基于群里已有照片改图（如把某人照片变成毕加索风格）",
        _make_handler("POST", "/v1/imagegen", timeout=30),
    ),
    (
        "send_video",
        {
            "type": "object",
            "properties": {
                "url": {"type": "string",
                         "description": "视频直链（http/https，指向 mp4 等视频文件的 URL，不是网页地址）。下载由 golem 完成，上限约 3MB"},
                "duration": {"type": "integer",
                              "description": "视频时长秒数（若已知，用于微信展示；不知道就别传）"},
            },
            "required": ["url"],
        },
        "在当前微信聊天发送一条视频：你给出视频文件直链，golem 负责下载校验并发送（上限约3MB小视频；每群每日有限额）。仅发送用户明确要求或群内上下文合理的内容，不主动搜寻分发来路不明的视频",
        _make_handler("POST", "/v1/video", timeout=60),
    ),
    (
        "reply_quote",
        {
            "type": "object",
            "properties": {
                "text": {"type": "string", "description": "引用回复的正文"},
            },
            "required": ["text"],
        },
        "在当前微信聊天以引用方式回复最近一条消息",
        _make_handler("POST", "/v1/quote", timeout=30),
    ),
    (
        "remember",
        {
            "type": "object",
            "properties": {
                "kind": {"type": "string", "enum": ["口头禅", "喜好", "身份", "关系", "flag", "名场面"],
                          "description": "记忆类别"},
                "content": {"type": "string", "description": "要记住的事实（关于当前发言人本人），一句话"},
            },
            "required": ["content"],
        },
        "记住关于当前发言人的一条稳定事实（下次对话可自然提起）",
        _make_handler("POST", "/v1/remember", timeout=20),
    ),
    (
        "get_member_profile",
        {
            "type": "object",
            "properties": {
                "name": {"type": "string", "description": "成员昵称或群昵称（支持部分匹配）"},
            },
            "required": ["name"],
        },
        "查询某位群成员的画像数据（人设+印象记忆），用于对话中自然提及或回答问题",
        _make_handler("POST", "/v1/profile", timeout=30),
    ),
    (
        "send_member_profile_card",
        {
            "type": "object",
            "properties": {
                "name": {"type": "string", "description": "成员昵称或群昵称（支持部分匹配）"},
            },
            "required": ["name"],
        },
        "把某位群成员的完整画像卡渲染成图片发到当前聊天（人设+印象记忆+活跃时段+表情倾向，与监控台同源）。用户要看某人的画像/人设卡时用它，不要自己拼内容",
        _make_handler("POST", "/v1/profile_card", timeout=150),
    ),
    (
        "get_group_members",
        {"type": "object", "properties": {}},
        "获取当前微信群成员列表（昵称/群昵称）",
        _make_handler("GET", "/v1/members", timeout=30),
    ),
    (
        "search_chat_history",
        {
            "type": "object",
            "properties": {
                "limit": {"type": "integer", "description": "返回条数，默认50"},
            },
        },
        "查询当前会话的历史消息（基于 statistics 插件的落库记录）",
        _make_handler("POST", "/v1/history", timeout=60),
    ),
    (
        "get_recent_context",
        {
            "type": "object",
            "properties": {
                "limit": {"type": "integer", "description": "返回条数，默认20"},
            },
        },
        "获取当前会话最近的消息上下文",
        _make_handler("GET", "/v1/context", timeout=30),
    ),
]


def register(ctx):
    registered = []
    for name, schema, description, handler in TOOLS:
        reg = ctx.register_tool(
            name=name,
            toolset="golem",
            schema=schema,
            handler=handler,
            description=description,
            emoji="💬",
        )
        if reg is not None:
            registered.append(name)
    ctx.register_system_prompt_section(
        id="golem_bridge_usage",
        content=USAGE_GUIDELINES,
        position="after_memory",
        max_chars=2000,
    )
    return registered
