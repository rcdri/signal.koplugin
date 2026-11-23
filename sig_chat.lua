--[[--
Chat view: messages as bubbles, a screenful at a time (newest at the
bottom), with replies, reactions, photos, files and sending.

@module koplugin.signal.chat
]]

local Blitbuffer = require("ffi/blitbuffer")
local ButtonDialog = require("ui/widget/buttondialog")
local ButtonTable = require("ui/widget/buttontable")
local CenterContainer = require("ui/widget/container/centercontainer")
local ConfirmBox = require("ui/widget/confirmbox")
local Device = require("device")
local Font = require("ui/font")
local FrameContainer = require("ui/widget/container/framecontainer")
local Geom = require("ui/geometry")
local GestureRange = require("ui/gesturerange")
local HorizontalGroup = require("ui/widget/horizontalgroup")
local HorizontalSpan = require("ui/widget/horizontalspan")
local ImageWidget = require("ui/widget/imagewidget")
local InfoMessage = require("ui/widget/infomessage")
local InputContainer = require("ui/widget/container/inputcontainer")
local InputDialog = require("ui/widget/inputdialog")
local LeftContainer = require("ui/widget/container/leftcontainer")
local LineWidget = require("ui/widget/linewidget")
local RightContainer = require("ui/widget/container/rightcontainer")
local Size = require("ui/size")
local TextBoxWidget = require("ui/widget/textboxwidget")
local TextViewer = require("ui/widget/textviewer")
local TextWidget = require("ui/widget/textwidget")
local TitleBar = require("ui/widget/titlebar")
local UIManager = require("ui/uimanager")
local VerticalGroup = require("ui/widget/verticalgroup")
local VerticalSpan = require("ui/widget/verticalspan")
local logger = require("logger")
local util = require("util")
local Client = require("sig_client")
local Core = require("sig_core")
local Format = require("sig_format")
local _ = require("gettext")
local T = require("ffi/util").template
local Screen = Device.screen

local ChatView = InputContainer:extend{
    name = "signal_chat",
    covers_fullscreen = true,
    chat = nil, -- chat table from /chats
    plugin = nil,
    on_close = nil,
}

local PAGE_SIZE = 50
local MAX_TEXT = 1500 -- longer messages get "Read more"

-- Signal's default reactions, as the helper names them.
ChatView.REACTIONS = { ":heart:", ":thumbsup:", ":thumbsdown:", ":joy:", ":open_mouth:", ":cry:" }
ChatView.QUICK_EMOJI = {
    ":smile:", ":joy:", ":heart:", ":thumbsup:", ":pray:", ":wink:", ":blush:", ":cry:",
    ":thinking:", ":tada:", ":fire:", ":ok_hand:", ":clap:", ":sob:", ":sweat_smile:", ":kiss:",
}

function ChatView:init()
    self.width = Screen:getWidth()
    self.height = Screen:getHeight()
    self.dimen = Geom:new{ x = 0, y = 0, w = self.width, h = self.height }
    local fs = Core:get("font_size", 19)
    self.text_face = Font:getFace("cfont", fs)
    self.name_face = Font:getFace("cfont", fs - 3)
    self.small_face = Font:getFace("cfont", math.max(12, fs - 5))
    self.sep_face = Font:getFace("cfont", math.max(12, fs - 4))
    self.msgs = {}
    self.by_id = {}
    self.wcache = {}
    self.more = true
    self.newer_unseen = 0
    self.typing = nil
    if Device:hasKeys() then
        self.key_events = {
            Close = { { Device.input.group.Back } },
            NextPage = { { Device.input.group.PgFwd } },
            PrevPage = { { Device.input.group.PgBack } },
        }
    end
    self.ges_events = {
        TapBody = { GestureRange:new{ ges = "tap", range = function() return self.body_dimen end } },
        HoldBody = { GestureRange:new{ ges = "hold", range = function() return self.body_dimen end } },
        SwipeBody = { GestureRange:new{ ges = "swipe", range = function() return self.body_dimen end } },
    }
    Core.open_chat = self.chat.id
    self.draft = Core.drafts and Core.drafts[self.chat.id]
    Core:addListener(self)
    self:loadInitial()
    self:build()
    self:markRead()
end

-- Data -----------------------------------------------------------------------------

function ChatView:index()
    self.by_id = {}
    for i, m in ipairs(self.msgs) do
        self.by_id[m.id] = i
    end
end

function ChatView:loadInitial()
    local res, err = Client.get("/messages", { chat = self.chat.id, limit = PAGE_SIZE }, 8)
    if not res then
        self.load_error = err
        self.msgs = {}
        self.more = false
    else
        self.load_error = nil
        self.msgs = res.messages or {}
        self.more = res.more
    end
    self.wcache = {}
    self:index()
    self.bottom = #self.msgs
    self.newer_unseen = 0
end

function ChatView:loadOlder()
    if not self.more or #self.msgs == 0 then return 0 end
    local first = self.msgs[1]
    local res, err = Client.get("/messages", {
        chat = self.chat.id, limit = PAGE_SIZE, before_ts = first.ts, before_id = first.id,
    }, 8)
    if not res then
        UIManager:show(InfoMessage:new{ text = err, timeout = 3 })
        return 0
    end
    local older = res.messages or {}
    self.more = res.more
    for i = #older, 1, -1 do
        table.insert(self.msgs, 1, older[i])
    end
    self:index()
    return #older
end

function ChatView:refreshChatInfo()
    local c = Client.get("/chat", { chat = self.chat.id }, 3)
    if c then
        self.chat = c
        Core.chat_cache[c.id] = c
    end
end

function ChatView:markRead()
    if (self.chat.unread or 0) > 0 or self._unread_dirty then
        self._unread_dirty = false
        self.chat.unread = 0
        Client.post("/read", { chat = self.chat.id }, 5)
    end
end

-- Message widgets ------------------------------------------------------------------

function ChatView:isGroup()
    return self.chat.kind == "group"
end

local function emoji_count_line(reactions)
    local order, counts = {}, {}
    for _, r in ipairs(reactions or {}) do
        if not counts[r.emoji] then
            counts[r.emoji] = 0
            table.insert(order, r.emoji)
        end
        counts[r.emoji] = counts[r.emoji] + 1
    end
    local parts = {}
    for _, e in ipairs(order) do
        table.insert(parts, counts[e] > 1 and (e .. " " .. counts[e]) or e)
    end
    return table.concat(parts, "  ")
end

function ChatView:mediaLine(md, m, idx)
    local label = Format.mediaLabel(md)
    local extra = {}
    if md.kind == "voice" or md.kind == "audio" or md.kind == "video" then
        if md.secs and md.secs > 0 then table.insert(extra, Format.duration(md.secs)) end
        table.insert(extra, _("play it on your phone"))
    elseif md.kind == "image" or md.kind == "gif" or md.kind == "sticker" then
        table.insert(extra, _("tap to view"))
    else
        if md.size and md.size > 0 then table.insert(extra, Format.size(md.size)) end
        table.insert(extra, _("tap to save"))
    end
    return "[" .. label .. "] " .. table.concat(extra, " · ")
end

--- Inline photo preview, if the helper already has it (never blocks on a download).
function ChatView:thumbnail(m, idx, max_w)
    if not Core:get("show_photos", true) then return nil end
    local md = m.media[idx]
    if not (md.kind == "image" or md.kind == "sticker" or md.kind == "gif") then return nil end
    local size = md.kind == "sticker" and math.floor(max_w * 0.45) or max_w
    local dir = Core.data_dir .. "/thumbs"
    util.makePath(dir)
    local path = string.format("%s/%s-%d-%d.jpg", dir, m.id:gsub("[^%w]", "_"), idx, size)
    if not util.fileExists(path) then
        local ok = Client.download("/media", {
            chat = m.chat, id = m.id, idx = idx - 1, max = size, cached = 1,
        }, path, 5)
        if not ok then return nil end
    end
    local ok, img = pcall(ImageWidget.new, ImageWidget, {
        file = path,
        width = nil,
        height = nil,
        scale_factor = 0,
        file_do_cache = false,
    })
    if not ok or not img then return nil end
    -- fit inside size × size*0.75 (stickers: square)
    local isz = img:getSize()
    local max_h = md.kind == "sticker" and size or math.floor(Screen:getHeight() * 0.25)
    if isz.w > size or isz.h > max_h then
        img:free()
        local scale = math.min(size / isz.w, max_h / isz.h)
        img = ImageWidget:new{ file = path, scale_factor = scale, file_do_cache = false }
    end
    return img
end

function ChatView:buildSystem(m)
    local text = m.text or ""
    if m.kind == "call" then text = "☎ " .. text end
    return CenterContainer:new{
        dimen = Geom:new{ w = self.width, h = 0 },
        TextBoxWidget:new{
            text = text .. "  · " .. Format.clock(m.ts),
            face = self.small_face,
            width = math.floor(self.width * 0.8),
            alignment = "center",
            fgcolor = Blitbuffer.COLOR_DARK_GRAY,
        },
    }
end

function ChatView:buildDay(ts)
    local text = TextWidget:new{ text = Format.day(ts), face = self.sep_face, bold = true }
    local tw = text:getSize().w
    local line_w = math.max(0, math.floor((self.width - tw) / 2) - Size.padding.large * 3)
    local line = function()
        return LineWidget:new{
            dimen = Geom:new{ w = line_w, h = Size.line.thin },
            background = Blitbuffer.COLOR_DARK_GRAY,
        }
    end
    return CenterContainer:new{
        dimen = Geom:new{ w = self.width, h = text:getSize().h + Size.padding.default * 2 },
        HorizontalGroup:new{
            align = "center",
            line(), HorizontalSpan:new{ width = Size.padding.large }, text,
            HorizontalSpan:new{ width = Size.padding.large }, line(),
        },
    }
end

function ChatView:buildBubble(m)
    local bg = m.from_me and Blitbuffer.gray(0.12) or Blitbuffer.COLOR_WHITE
    local max_w = math.floor(self.width * 0.80)
    local pad = Size.padding.default + Size.padding.small
    local inner_max = max_w - 2 * pad - 2 * Size.border.thin
    local parts = {}
    local natural = 0 -- widest content, for shrink-to-fit
    local function add(w, grow)
        table.insert(parts, w)
        local sw = w:getSize().w
        if grow ~= false and sw > natural then natural = sw end
    end
    if self:isGroup() and not m.from_me and m.sender_name then
        add(TextWidget:new{ text = m.sender_name, face = self.name_face, bold = true, max_width = inner_max })
    end
    if m.reply then
        local rtext = Format.oneLine(m.reply.text or "", 140)
        local who = m.reply.sender_name or (m.reply.id == "" and _("Story") or "")
        local qbox = VerticalGroup:new{
            align = "left",
            TextWidget:new{ text = who, face = self.small_face, bold = true, max_width = inner_max - Size.padding.large },
            TextBoxWidget:new{ bgcolor = bg,
                text = rtext ~= "" and rtext or "…",
                face = self.small_face,
                width = inner_max - Size.padding.large * 2,
                height = self.small_face.size * 2.8,
                height_overflow_show_ellipsis = true,
                fgcolor = Blitbuffer.COLOR_DARK_GRAY,
            },
        }
        local qh = qbox:getSize().h
        add(HorizontalGroup:new{
            align = "top",
            LineWidget:new{ dimen = Geom:new{ w = Screen:scaleBySize(3), h = qh }, background = Blitbuffer.COLOR_DARK_GRAY },
            HorizontalSpan:new{ width = Size.padding.default },
            qbox,
        })
        add(VerticalSpan:new{ width = Size.padding.small }, false)
    end
    if m.kind == "deleted" then
        add(TextWidget:new{
            text = m.from_me and _("You deleted this message.") or _("This message was deleted."),
            face = self.text_face, fgcolor = Blitbuffer.COLOR_DARK_GRAY, max_width = inner_max,
        })
    else
        for i, md in ipairs(m.media or {}) do
            local thumb = self:thumbnail(m, i, inner_max)
            if thumb then
                add(thumb)
            else
                add(TextBoxWidget:new{ bgcolor = bg, text = self:mediaLine(md, m, i), face = self.small_face, width = inner_max, bold = true })
            end
        end
        local text = m.text or ""
        if m.kind == "sticker" then text = "" end
        if text ~= "" then
            local truncated = false
            if #text > MAX_TEXT then
                text = text:sub(1, MAX_TEXT):gsub("[\128-\191]*$", ""):gsub("[\192-\255]$", "") .. "…"
                truncated = true
            end
            -- short single lines: bubble shrinks to the text
            local single = not text:find("\n") and TextWidget:new{ text = text, face = self.text_face }
            if single and single:getSize().w <= inner_max then
                add(single)
            else
                if single then single:free() end
                -- keep every bubble well inside one screen
                local max_text_h = math.floor(self.height * 0.5) - (m.media and #m.media > 0 and math.floor(self.height * 0.25) or 0)
                local tb = TextBoxWidget:new{ bgcolor = bg, text = text, face = self.text_face, width = inner_max }
                if tb:getSize().h > max_text_h then
                    tb:free()
                    tb = TextBoxWidget:new{
                        bgcolor = bg, text = text, face = self.text_face, width = inner_max,
                        height = max_text_h, height_overflow_show_ellipsis = true,
                    }
                    truncated = true
                end
                add(tb)
            end
            if truncated then
                m._truncated = true
                add(TextWidget:new{ text = _("[Read more]"), face = self.small_face, bold = true })
            end
        end
    end
    if m.reactions and #m.reactions > 0 then
        add(VerticalSpan:new{ width = Size.padding.small }, false)
        add(TextBoxWidget:new{ bgcolor = bg, text = emoji_count_line(m.reactions), face = self.small_face, width = inner_max, bold = true }, false)
    end
    -- footer: time, edited, timer, ticks
    local foot = { Format.clock(m.ts) }
    if m.edited then table.insert(foot, _("edited")) end
    if m.expires_in and m.expires_in > 0 then table.insert(foot, "⏱ " .. Format.expiry(m.expires_in)) end
    if m.from_me then
        local mark = Format.statusMark(m)
        if m.status == "failed" then mark = mark .. " — " .. _("tap to retry") end
        if mark ~= "" then table.insert(foot, mark) end
    end
    local foot_w = TextWidget:new{
        text = table.concat(foot, " · "),
        face = self.small_face,
        fgcolor = m.status == "failed" and Blitbuffer.COLOR_BLACK or Blitbuffer.COLOR_DARK_GRAY,
        bold = m.status == "failed",
        max_width = inner_max,
    }
    natural = math.max(natural, foot_w:getSize().w)
    local content_w = math.min(inner_max, natural)
    local vg = VerticalGroup:new{ align = "left" }
    for _, p in ipairs(parts) do table.insert(vg, p) end
    table.insert(vg, VerticalSpan:new{ width = Screen:scaleBySize(2) })
    table.insert(vg, RightContainer:new{ dimen = Geom:new{ w = content_w, h = foot_w:getSize().h }, foot_w })
    local bubble = FrameContainer:new{
        bordersize = m.mentions_me and Size.border.thick or Size.border.thin,
        radius = Screen:scaleBySize(10),
        padding = pad,
        padding_top = Size.padding.small + Size.padding.small,
        padding_bottom = Size.padding.small,
        background = bg,
        vg,
    }
    local bsize = bubble:getSize()
    local margin = Size.padding.large
    local row_w = self.width - 2 * margin
    local holder
    if m.from_me then
        holder = RightContainer:new{ dimen = Geom:new{ w = row_w, h = bsize.h }, bubble }
    else
        holder = LeftContainer:new{ dimen = Geom:new{ w = row_w, h = bsize.h }, bubble }
    end
    return HorizontalGroup:new{ HorizontalSpan:new{ width = margin }, holder }
end

function ChatView:widgetFor(i)
    local m = self.msgs[i]
    local prev = self.msgs[i - 1]
    local key = m.id .. "|" .. (prev and Format.sameDay(prev.ts, m.ts) and "s" or "d")
    local c = self.wcache[key]
    if c then return c end
    local items = {}
    if not prev or not Format.sameDay(prev.ts, m.ts) then
        table.insert(items, self:buildDay(m.ts))
    end
    local w
    if m.kind == "system" or m.kind == "call" then
        w = self:buildSystem(m)
    else
        w = self:buildBubble(m)
    end
    local day_h = items[1] and items[1]:getSize().h or 0
    table.insert(items, w)
    local group = VerticalGroup:new{ align = "left", unpack(items) }
    -- a little air between bubbles; more when the sender changes
    local gap = Size.padding.small
    if prev and prev.sender ~= m.sender then gap = Size.padding.default end
    c = { widget = group, h = group:getSize().h + gap, gap = gap, day_h = day_h, msg = m }
    self.wcache[key] = c
    return c
end

function ChatView:invalidate(id)
    for k in pairs(self.wcache) do
        if k:sub(1, #id + 1) == id .. "|" then
            self.wcache[k] = nil
        end
    end
end

-- Layout ---------------------------------------------------------------------------

function ChatView:subtitle()
    local parts = {}
    if self.typing then
        return T(_("%1 is typing…"), self.typing)
    end
    local st = Core.status
    if st.state == "disconnected" or st.state == "helper_down" or st.state == "error" then
        table.insert(parts, _("offline"))
    elseif st.state == "connecting" then
        table.insert(parts, _("connecting…"))
    end
    if self.chat.kind == "group" and (self.chat.members or 0) > 0 then
        table.insert(parts, T(_("%1 members"), self.chat.members))
    elseif self.chat.kind == "dm" and self.chat.phone and self.chat.phone ~= "" then
        table.insert(parts, self.chat.phone)
    end
    if (self.chat.expire_timer or 0) > 0 then
        table.insert(parts, "⏱ " .. Format.expiry(self.chat.expire_timer))
    end
    if self.chat.read_only then table.insert(parts, _("only admins can send")) end
    return #parts > 0 and table.concat(parts, " · ") or " "
end

function ChatView:build()
    self.title_bar = TitleBar:new{
        width = self.width,
        fullscreen = true,
        with_bottom_line = true,
        title = self.chat.name or "",
        subtitle = self:subtitle(),
        left_icon = "chevron.left",
        left_icon_tap_callback = function() self:onClose() end,
        right_icon = "appbar.menu",
        right_icon_tap_callback = function() self:chatMenu() end,
        show_parent = self,
    }
    local footer = self:buildFooter()
    local th, fh = self.title_bar:getSize().h, footer:getSize().h
    local body_h = self.height - th - fh
    self.body_dimen = Geom:new{ x = 0, y = th, w = self.width, h = body_h }
    local page = self:layoutPage(body_h)
    -- footer depends on whether there's more above/below: rebuild it now
    footer = self:buildFooter()
    self[1] = FrameContainer:new{
        width = self.width,
        height = self.height,
        background = Blitbuffer.COLOR_WHITE,
        bordersize = 0,
        padding = 0,
        VerticalGroup:new{
            align = "left",
            self.title_bar,
            page,
            footer,
        },
    }
end

--- Fills the body upwards from self.bottom. Sets self.top and hit areas.
function ChatView:layoutPage(body_h)
    local avail = body_h - Size.padding.default
    local stack = {}
    local used = 0
    local top = self.bottom + 1
    local i = self.bottom
    while i >= 1 do
        local c = self:widgetFor(i)
        if used + c.h > avail and #stack > 0 then break end
        table.insert(stack, 1, c)
        used = used + c.h
        top = i
        i = i - 1
        if used >= avail then break end
    end
    self.top = top
    local vg = VerticalGroup:new{ align = "left" }
    local banner
    if #self.msgs == 0 then
        local text
        if self.load_error then
            text = _("Couldn't load messages: ") .. self.load_error
        else
            text = _("No messages on this Kindle yet.\nSignal only gives linked devices messages from the moment they're linked (plus any history your phone transferred).")
        end
        banner = TextBoxWidget:new{
            text = text, face = self.small_face, width = math.floor(self.width * 0.8), alignment = "center",
            fgcolor = Blitbuffer.COLOR_DARK_GRAY,
        }
    elseif self.top == 1 and not self.more and used + self.small_face.size * 3 < avail then
        banner = TextBoxWidget:new{
            text = _("Start of the messages on this Kindle"),
            face = self.small_face, width = math.floor(self.width * 0.8), alignment = "center",
            fgcolor = Blitbuffer.COLOR_DARK_GRAY,
        }
    end
    local banner_h = 0
    if banner then
        local b = CenterContainer:new{
            dimen = Geom:new{ w = self.width, h = banner:getSize().h + Size.padding.large * 2 },
            banner,
        }
        banner_h = b:getSize().h
        table.insert(vg, b)
    end
    -- hit areas, in screen coordinates (content sits at the bottom of the body)
    local y = self.body_dimen.y + body_h - used - Size.padding.default
    if #self.msgs == 0 then y = self.body_dimen.y + banner_h end
    self.hits = {}
    for _, c in ipairs(stack) do
        table.insert(self.hits, { y0 = y + c.day_h, y1 = y + c.h, msg = c.msg })
        y = y + c.h
        table.insert(vg, c.widget)
        table.insert(vg, VerticalSpan:new{ width = c.gap })
    end
    local BottomContainer = require("ui/widget/container/bottomcontainer")
    local container_class = #self.msgs == 0 and require("ui/widget/container/topcontainer") or BottomContainer
    return FrameContainer:new{
        bordersize = 0,
        padding = 0,
        width = self.width,
        height = body_h,
        container_class:new{
            dimen = Geom:new{ w = self.width, h = body_h - (#self.msgs == 0 and 0 or Size.padding.default) },
            vg,
        },
    }
end

function ChatView:canWrite()
    return not self.chat.read_only and Core:isLinked()
end

function ChatView:buildFooter()
    local older_ok = (self.top or 1) > 1 or self.more
    local newer_ok = (self.bottom or 0) < #self.msgs
    local newer_text = "▼"
    if self.newer_unseen > 0 then newer_text = T(_("▼ %1 new"), self.newer_unseen) end
    local row = {
        { text = "▲", enabled = older_ok, callback = function() self:onPrevPage() end },
        {
            text = self.reply_to and _("Write reply") or _("Write"),
            enabled = self:canWrite(),
            callback = function() self:compose() end,
        },
        { text = newer_text, enabled = newer_ok, callback = function() self:onNextPage() end },
    }
    return VerticalGroup:new{
        LineWidget:new{ dimen = Geom:new{ w = self.width, h = Size.line.medium }, background = Blitbuffer.COLOR_DARK_GRAY },
        ButtonTable:new{ width = self.width, buttons = { row }, zero_sep = true, show_parent = self },
    }
end

function ChatView:render(mode)
    if self.closed then return end
    self:build()
    UIManager:setDirty(self, mode or "ui")
end

function ChatView:atBottom()
    return self.bottom >= #self.msgs
end

-- Paging ---------------------------------------------------------------------------

function ChatView:onPrevPage()
    if (self.top or 1) > 1 then
        self.bottom = self.top - 1
    elseif self.more then
        local n = self:loadOlder()
        if n == 0 then return true end
        self.bottom = n
    else
        return true
    end
    self:render("partial")
    return true
end

function ChatView:onNextPage()
    if self:atBottom() then return true end
    -- next page starts right after the current bottom
    local avail = self.body_dimen.h - Size.padding.default
    local used, i = 0, self.bottom + 1
    local last = self.bottom + 1
    while i <= #self.msgs do
        local c = self:widgetFor(i)
        if used + c.h > avail and i > self.bottom + 1 then break end
        used = used + c.h
        last = i
        i = i + 1
    end
    self.bottom = last
    if self:atBottom() then self.newer_unseen = 0 end
    self:render("partial")
    return true
end

function ChatView:onSwipeBody(_, ges)
    if ges.direction == "south" or ges.direction == "east" then
        self:onPrevPage()
    elseif ges.direction == "north" or ges.direction == "west" then
        self:onNextPage()
    end
    return true
end

function ChatView:msgAt(pos)
    if not pos or not self.hits then return end
    for _, h in ipairs(self.hits) do
        if pos.y >= h.y0 and pos.y < h.y1 then return h.msg end
    end
end

function ChatView:onTapBody(_, ges)
    local m = self:msgAt(ges.pos)
    if not m then return true end
    if m.from_me and m.status == "failed" then
        self:retry(m)
    elseif m.media and m.media[1] and (m.media[1].kind == "image" or m.media[1].kind == "gif" or m.media[1].kind == "sticker") then
        self:openMedia(m, 1)
    elseif m._truncated then
        self:showFull(m)
    else
        self:messageActions(m)
    end
    return true
end

function ChatView:onHoldBody(_, ges)
    local m = self:msgAt(ges.pos)
    if m then self:messageActions(m) end
    return true
end

-- Actions --------------------------------------------------------------------------

function ChatView:showFull(m)
    UIManager:show(TextViewer:new{
        title = m.from_me and _("You") or (m.sender_name or self.chat.name),
        text = m.text or "",
        justified = false,
    })
end

function ChatView:messageActions(m)
    if m.kind == "system" or m.kind == "call" then return end
    local dialog
    local function close() UIManager:close(dialog) end
    local buttons = {}
    local deleted = m.kind == "deleted"
    if not deleted then
        for i, md in ipairs(m.media or {}) do
            local label
            if md.kind == "image" or md.kind == "gif" or md.kind == "sticker" then
                label = _("View photo")
                if md.kind == "sticker" then label = _("View sticker") end
            elseif md.kind == "file" or md.kind == "audio" or md.kind == "video" or md.kind == "voice" then
                label = T(_("Save %1 to the Kindle"), Format.mediaLabel(md))
            end
            if label then
                table.insert(buttons, { { text = label, callback = function() close() self:openMedia(m, i) end } })
            end
        end
        if self:canWrite() and m.id ~= "" then
            table.insert(buttons, {
                { text = _("Reply"), callback = function() close() self.reply_to = m self:compose() end },
                { text = _("React"), callback = function() close() self:reactDialog(m) end },
            })
        end
        if m._truncated then
            table.insert(buttons, { { text = _("Read the whole message"), callback = function() close() self:showFull(m) end } })
        end
        if (m.text or "") ~= "" then
            table.insert(buttons, { { text = _("Copy text"), callback = function()
                close()
                if Device.input and Device.input.setClipboardText then
                    Device.input.setClipboardText(m.text)
                    UIManager:show(InfoMessage:new{ text = _("Copied."), timeout = 1 })
                end
            end } })
        end
    end
    if m.from_me and m.status == "failed" then
        table.insert(buttons, { { text = _("Try again"), callback = function() close() self:retry(m) end } })
    end
    if m.from_me and not deleted and (m.kind == "text") and m.status ~= "failed" and m.status ~= "pending"
        and os.time() - m.ts / 1000 < 24 * 3600 then
        table.insert(buttons, { { text = _("Edit"), callback = function() close() self:compose(m) end } })
    end
    table.insert(buttons, { { text = _("Delete…"), callback = function() close() self:deleteDialog(m) end } })
    table.insert(buttons, { { text = _("Details"), callback = function() close() self:details(m) end } })
    dialog = ButtonDialog:new{
        title = Format.oneLine(Format.preview(m), 120),
        title_align = "center",
        buttons = buttons,
    }
    UIManager:show(dialog)
end

function ChatView:details(m)
    local lines = {
        T(_("From: %1"), m.from_me and _("You") or (m.sender_name or "?")),
        T(_("Sent: %1"), os.date("%Y-%m-%d %H:%M:%S", math.floor(m.ts / 1000))),
    }
    if m.from_me and m.status then table.insert(lines, T(_("Status: %1"), m.status)) end
    if m.error then table.insert(lines, T(_("Error: %1"), m.error)) end
    if m.expires_in and m.expires_in > 0 then
        table.insert(lines, T(_("Disappears: %1 after reading"), Format.expiry(m.expires_in)))
    end
    for _, r in ipairs(m.reactions or {}) do
        table.insert(lines, T(_("%1 reacted %2"), r.name or "?", r.emoji))
    end
    for _, md in ipairs(m.media or {}) do
        local s = Format.mediaLabel(md)
        if md.mime then s = s .. " (" .. md.mime .. ")" end
        if md.size and md.size > 0 then s = s .. " " .. Format.size(md.size) end
        if md.w and md.w > 0 then s = s .. " " .. md.w .. "×" .. (md.h or 0) end
        table.insert(lines, s)
    end
    UIManager:show(InfoMessage:new{ text = table.concat(lines, "\n") })
end

function ChatView:reactDialog(m)
    local dialog
    local mine
    for _, r in ipairs(m.reactions or {}) do
        if r.name == "You" then mine = r.emoji end
    end
    local function react(e)
        UIManager:close(dialog)
        local res, err = Client.post("/react", { chat = m.chat, id = m.id, emoji = e }, 5)
        if not res then UIManager:show(InfoMessage:new{ text = err }) end
    end
    local row1, row2 = {}, {}
    for i, e in ipairs(ChatView.REACTIONS) do
        local btn = { text = e == mine and ("[" .. e .. "]") or e, callback = function() react(e) end }
        table.insert(i <= 3 and row1 or row2, btn)
    end
    local buttons = { row1, row2 }
    table.insert(buttons, { { text = _("Other emoji…"), callback = function()
        UIManager:close(dialog)
        local input
        input = InputDialog:new{
            title = _("React with"),
            input_hint = ":fire:",
            description = _("Type an emoji name between colons, like :fire: or :sparkles:."),
            buttons = { {
                { text = _("Cancel"), id = "close", callback = function() UIManager:close(input) end },
                { text = _("React"), is_enter_default = true, callback = function()
                    local e = input:getInputText():gsub("^%s+", ""):gsub("%s+$", "")
                    UIManager:close(input)
                    if e ~= "" then
                        if not e:match("^:") then e = ":" .. e .. ":" end
                        local res, err = Client.post("/react", { chat = m.chat, id = m.id, emoji = e }, 5)
                        if not res then UIManager:show(InfoMessage:new{ text = err }) end
                    end
                end },
            } },
        }
        UIManager:show(input)
        input:onShowKeyboard()
    end } })
    if mine then
        table.insert(buttons, { { text = _("Remove my reaction"), callback = function() react(mine) end } })
    end
    dialog = ButtonDialog:new{ title = _("React"), title_align = "center", buttons = buttons }
    UIManager:show(dialog)
end

function ChatView:deleteDialog(m)
    local dialog
    local buttons = {
        { { text = _("Delete on this Kindle"), callback = function()
            UIManager:close(dialog)
            Client.post("/delete", { chat = m.chat, id = m.id, for_everyone = false }, 5)
        end } },
    }
    if m.from_me and m.kind ~= "deleted" and m.status ~= "failed" and m.status ~= "pending" then
        table.insert(buttons, { { text = _("Delete for everyone"), callback = function()
            UIManager:close(dialog)
            UIManager:show(ConfirmBox:new{
                text = _("Delete this message for everyone in the chat?"),
                ok_text = _("Delete"),
                ok_callback = function()
                    local res, err = Client.post("/delete", { chat = m.chat, id = m.id, for_everyone = true }, 5)
                    if not res then UIManager:show(InfoMessage:new{ text = err }) end
                end,
            })
        end } })
    end
    table.insert(buttons, { { text = _("Cancel"), callback = function() UIManager:close(dialog) end } })
    dialog = ButtonDialog:new{ title = _("Delete message"), title_align = "center", buttons = buttons }
    UIManager:show(dialog)
end

function ChatView:retry(m)
    local res, err = Client.post("/send", { chat = m.chat, text = m.text, retry_id = m.id,
        reply_to = m.reply and m.reply.id or nil }, 10)
    if not res then
        UIManager:show(InfoMessage:new{ text = err })
        return
    end
    self:remove(m.id)
    self:upsert(res)
    self.bottom = #self.msgs
    self:render("ui")
end

--- Opens a photo, or saves a file into the Signal folder.
function ChatView:openMedia(m, idx)
    local md = m.media[idx]
    if not md then return end
    local is_image = md.kind == "image" or md.kind == "gif" or md.kind == "sticker"
    local wait = InfoMessage:new{ text = is_image and _("Loading photo…") or _("Downloading…") }
    UIManager:show(wait)
    UIManager:forceRePaint()
    local ok, err, dest
    if is_image then
        local dir = Core.data_dir .. "/view"
        util.makePath(dir)
        dest = dir .. "/photo.jpg"
        ok, err = Client.download("/media", {
            chat = m.chat, id = m.id, idx = idx - 1, max = math.max(Screen:getWidth(), Screen:getHeight()),
        }, dest, 180)
    else
        dest = self.plugin:downloadPath(md, m)
        ok, err = Client.download("/media", { chat = m.chat, id = m.id, idx = idx - 1 }, dest, 300)
    end
    UIManager:close(wait)
    if not ok then
        UIManager:show(InfoMessage:new{ text = T(_("Couldn't get the file: %1"), err or "?") })
        return
    end
    if is_image then
        local ImageViewer = require("ui/widget/imageviewer")
        UIManager:show(ImageViewer:new{
            file = dest,
            fullscreen = true,
            with_title_bar = true,
            title_text = m.from_me and _("You") or (m.sender_name or self.chat.name),
            caption = m.text,
        })
        -- the thumbnail can now be shown inline too
        self:invalidate(m.id)
        self:render("ui")
    else
        self.plugin:offerOpen(dest)
    end
end

-- Composing ------------------------------------------------------------------------

function ChatView:compose(edit_msg)
    local reply = self.reply_to
    local title
    if edit_msg then
        title = _("Edit message")
    elseif reply then
        title = T(_("Reply to %1"), reply.from_me and _("yourself") or (reply.sender_name or self.chat.name))
    else
        title = self.chat.kind == "self" and _("Note to Self") or T(_("Message %1"), self.chat.name)
    end
    local dialog
    local function finish()
        UIManager:close(dialog)
    end
    local buttons = { {
        { text = _("Cancel"), id = "close", callback = function()
            if not edit_msg then self.draft = dialog:getInputText() end
            self.reply_to = nil
            finish()
            self:render("ui")
        end },
        { text = "☺", callback = function() self:emojiPicker(dialog) end },
        { text = edit_msg and _("Save") or _("Send"), is_enter_default = false, callback = function()
            local text = dialog:getInputText()
            if text:match("^%s*$") then return end
            finish()
            if edit_msg then
                local res, err = Client.post("/edit", { chat = self.chat.id, id = edit_msg.id, text = text }, 8)
                if not res then UIManager:show(InfoMessage:new{ text = err }) end
                return
            end
            self.draft = nil
            self.reply_to = nil
            self:send(text, reply)
        end },
    } }
    dialog = InputDialog:new{
        title = title,
        description = reply and Format.oneLine(Format.preview(reply), 100) or nil,
        input = edit_msg and edit_msg.text or self.draft or "",
        input_hint = _("Signal message"),
        allow_newline = true,
        text_height = Screen:scaleBySize(140),
        buttons = buttons,
    }
    UIManager:show(dialog)
    dialog:onShowKeyboard()
end

function ChatView:emojiPicker(input)
    local picker
    local rows, row = {}, {}
    for _, e in ipairs(ChatView.QUICK_EMOJI) do
        table.insert(row, { text = e, callback = function()
            UIManager:close(picker)
            input:addTextToInput(e .. " ")
        end })
        if #row == 4 then
            table.insert(rows, row)
            row = {}
        end
    end
    if #row > 0 then table.insert(rows, row) end
    picker = ButtonDialog:new{
        title = _("Emoji are written as :names: and sent as real emoji."),
        title_align = "center",
        buttons = rows,
    }
    UIManager:show(picker)
end

function ChatView:send(text, reply)
    local res, err = Client.post("/send", { chat = self.chat.id, text = text, reply_to = reply and reply.id or nil }, 10)
    if not res then
        self.draft = text
        UIManager:show(InfoMessage:new{ text = T(_("Not sent: %1"), err or "?") })
        return
    end
    self.reply_to = nil
    self:upsert(res)
    self.bottom = #self.msgs
    self.newer_unseen = 0
    self:render("ui")
end

-- Live updates ---------------------------------------------------------------------

--- Inserts or replaces a message (kept in time order).
function ChatView:upsert(m)
    local i = self.by_id[m.id]
    if i then
        self.msgs[i] = m
        self:invalidate(m.id)
        return false
    end
    local pos = #self.msgs + 1
    while pos > 1 and self.msgs[pos - 1].ts > m.ts do
        pos = pos - 1
    end
    table.insert(self.msgs, pos, m)
    if pos <= self.bottom then self.bottom = self.bottom + 1 end
    -- the next message's day separator may change
    if self.msgs[pos + 1] then self:invalidate(self.msgs[pos + 1].id) end
    self:index()
    return true
end

function ChatView:remove(id)
    local i = self.by_id[id]
    if not i then return end
    table.remove(self.msgs, i)
    if self.bottom >= i then self.bottom = math.max(0, self.bottom - 1) end
    if self.msgs[i] then self:invalidate(self.msgs[i].id) end
    self:invalidate(id)
    self:index()
end

function ChatView:scheduleRender()
    if self._render_pending then return end
    self._render_pending = true
    UIManager:scheduleIn(0.3, function()
        self._render_pending = false
        self:render("ui")
    end)
end

function ChatView:onSignalEvent(ev)
    if self.closed then return end
    local mine = ev.chat == self.chat.id
    if ev.type == "msg" and mine and ev.msg then
        local was_bottom = self:atBottom()
        local added = self:upsert(ev.msg)
        if added then
            if was_bottom then
                self.bottom = #self.msgs
            elseif not ev.msg.from_me then
                self.newer_unseen = self.newer_unseen + 1
            end
            if not ev.msg.from_me then
                self._unread_dirty = true
                self:markRead()
            end
            self.typing = nil
        end
        self:scheduleRender()
    elseif ev.type == "remove" and mine and ev.msg then
        self:remove(ev.msg.id)
        self:scheduleRender()
    elseif ev.type == "media" and mine and ev.msg then
        self:invalidate(ev.msg.id)
        self:scheduleRender()
    elseif ev.type == "chat" and mine then
        self:refreshChatInfo()
        if ev.reload then self:loadInitial() end
        self:scheduleRender()
    elseif ev.type == "typing" and mine then
        local name
        if ev.new then
            local c = Core.chat_cache[ev.text]
            name = (c and c.name) or (self.chat.kind == "dm" and self.chat.name) or _("Someone")
            if self.chat.kind == "group" then
                for i = #self.msgs, 1, -1 do
                    if self.msgs[i].sender == ev.text and self.msgs[i].sender_name then
                        name = self.msgs[i].sender_name
                        break
                    end
                end
            end
        end
        if name ~= self.typing then
            self.typing = name
            self:scheduleRender()
            if name then
                UIManager:unschedule(self._typing_off)
                self._typing_off = self._typing_off or function()
                    self.typing = nil
                    self:scheduleRender()
                end
                UIManager:scheduleIn(15, self._typing_off)
            end
        end
    elseif ev.type == "error" and mine then
        UIManager:show(InfoMessage:new{ text = ev.text, timeout = 4 })
    elseif ev.type == "names" or ev.type == "reload" then
        local keep_bottom = self:atBottom()
        local old_bottom_id = self.msgs[self.bottom] and self.msgs[self.bottom].id
        self:refreshChatInfo()
        self:loadInitial()
        if not keep_bottom and old_bottom_id and self.by_id[old_bottom_id] then
            self.bottom = self.by_id[old_bottom_id]
        end
        self:scheduleRender()
    elseif ev.type == "status" then
        self:scheduleRender()
    end
end

function ChatView:chatMenu()
    self.plugin:chatMenu(self)
end

function ChatView:onClose()
    if self.closed then return true end
    self.closed = true
    Core.drafts = Core.drafts or {}
    Core.drafts[self.chat.id] = (self.draft and self.draft ~= "") and self.draft or nil
    if Core.open_chat == self.chat.id then Core.open_chat = nil end
    Core:removeListener(self)
    UIManager:close(self)
    if self.on_close then self.on_close() end
    return true
end

function ChatView:onCloseWidget()
    for _, c in pairs(self.wcache) do
        if c.widget and c.widget.free then pcall(c.widget.free, c.widget) end
    end
    self.wcache = {}
end

return ChatView
