--[[--
Text helpers: times, previews, status marks.

@module koplugin.signal.format
]]

local _ = require("gettext")
local T = require("ffi/util").template

local Format = {}

local function days_between(a, b)
    local da = os.date("*t", a)
    local db = os.date("*t", b)
    local ta = os.time{ year = da.year, month = da.month, day = da.day, hour = 12 }
    local tb = os.time{ year = db.year, month = db.month, day = db.day, hour = 12 }
    return math.floor((tb - ta) / 86400 + 0.5)
end

--- Short time for lists: 14:05, Yesterday, Mon, 12 Nov, 12/11/24.
function Format.listTime(ms)
    if not ms or ms <= 0 then return "" end
    local t = math.floor(ms / 1000)
    local now = os.time()
    local d = days_between(t, now)
    if d <= 0 then
        return os.date("%H:%M", t)
    elseif d == 1 then
        return _("Yesterday")
    elseif d < 7 then
        return os.date("%a", t)
    elseif os.date("%Y", t) == os.date("%Y", now) then
        return os.date("%d %b", t):gsub("^0", "")
    end
    return os.date("%d/%m/%y", t)
end

function Format.clock(ms)
    return os.date("%H:%M", math.floor(ms / 1000))
end

--- Day separator text: Today, Yesterday, Monday 10 November.
function Format.day(ms)
    local t = math.floor(ms / 1000)
    local d = days_between(t, os.time())
    if d <= 0 then return _("Today") end
    if d == 1 then return _("Yesterday") end
    if d < 7 then return os.date("%A", t) end
    local s = os.date("%A %d %B", t):gsub(" 0", " ")
    if os.date("%Y", t) ~= os.date("%Y") then s = s .. os.date(" %Y", t) end
    return s
end

function Format.sameDay(a, b)
    return os.date("%Y%m%d", math.floor(a / 1000)) == os.date("%Y%m%d", math.floor(b / 1000))
end

function Format.oneLine(s, max)
    s = (s or ""):gsub("%s+", " "):gsub("^ ", ""):gsub(" $", "")
    if max and #s > max then
        -- cut on a UTF-8 boundary
        local cut = max
        while cut > 0 and s:byte(cut + 1) and s:byte(cut + 1) >= 0x80 and s:byte(cut + 1) < 0xC0 do
            cut = cut - 1
        end
        s = s:sub(1, cut) .. "…"
    end
    return s
end

function Format.size(n)
    if not n or n <= 0 then return "" end
    if n < 1024 then return n .. " B" end
    if n < 1024 * 1024 then return string.format("%.0f KB", n / 1024) end
    return string.format("%.1f MB", n / 1024 / 1024)
end

function Format.duration(secs)
    if not secs or secs <= 0 then return "" end
    return string.format("%d:%02d", math.floor(secs / 60), secs % 60)
end

local media_labels = {
    image = _("Photo"), gif = _("GIF"), video = _("Video"), voice = _("Voice message"),
    audio = _("Audio"), sticker = _("Sticker"), file = _("File"),
}

function Format.mediaLabel(m)
    local label = media_labels[m.kind] or _("File")
    if m.kind == "file" and m.name and m.name ~= "" then
        label = m.name
    end
    return label
end

--- One-line summary of a message (chat list, notifications, replies).
function Format.preview(msg)
    if msg.kind == "deleted" then return _("This message was deleted") end
    local text = msg.text or ""
    if msg.media and msg.media[1] then
        local label = "[" .. Format.mediaLabel(msg.media[1]) .. "]"
        text = text ~= "" and (label .. " " .. text) or label
    end
    if text == "" then return _("[Message]") end
    return text
end

--- Notification / list title for a chat: group name, or the sender.
function Format.chatTitle(chat_id, msg)
    local Core = require("sig_core")
    local c = Core.chat_cache and Core.chat_cache[chat_id]
    if c and c.kind == "group" then
        local who = msg and msg.sender_name
        return who and T(_("%1 in %2"), who, c.name) or c.name
    end
    if c and c.name and c.name ~= "" then return c.name end
    return msg and msg.sender_name or _("Signal")
end

--- Delivery mark for my messages.
function Format.statusMark(msg)
    local s = msg.status
    if s == "pending" then return "◷"
    elseif s == "sent" then return "✓"
    elseif s == "delivered" then return "✓✓"
    elseif s == "read" then return "✓✓ " .. _("read")
    elseif s == "failed" then return "⚠ " .. _("not sent")
    end
    return ""
end

function Format.expiry(secs)
    if not secs or secs <= 0 then return nil end
    if secs % 604800 == 0 then return T(_("%1 w"), secs / 604800) end
    if secs % 86400 == 0 then return T(_("%1 d"), secs / 86400) end
    if secs % 3600 == 0 then return T(_("%1 h"), secs / 3600) end
    if secs % 60 == 0 then return T(_("%1 min"), secs / 60) end
    return T(_("%1 s"), secs)
end

--- Strips the colons of :emoji_names: for display ("thumbsup" stays readable).
function Format.reactionText(emoji)
    return emoji or ""
end

return Format
