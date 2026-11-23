--[[--
Chat list: chats with previews and unread counts, a page at a time.

@module koplugin.signal.chatlist
]]

local Blitbuffer = require("ffi/blitbuffer")
local ButtonDialog = require("ui/widget/buttondialog")
local ButtonTable = require("ui/widget/buttontable")
local ConfirmBox = require("ui/widget/confirmbox")
local Device = require("device")
local Font = require("ui/font")
local FrameContainer = require("ui/widget/container/framecontainer")
local Geom = require("ui/geometry")
local GestureRange = require("ui/gesturerange")
local HorizontalGroup = require("ui/widget/horizontalgroup")
local HorizontalSpan = require("ui/widget/horizontalspan")
local InfoMessage = require("ui/widget/infomessage")
local InputContainer = require("ui/widget/container/inputcontainer")
local InputDialog = require("ui/widget/inputdialog")
local LeftContainer = require("ui/widget/container/leftcontainer")
local LineWidget = require("ui/widget/linewidget")
local Menu = require("ui/widget/menu")
local OverlapGroup = require("ui/widget/overlapgroup")
local RightContainer = require("ui/widget/container/rightcontainer")
local Size = require("ui/size")
local TextWidget = require("ui/widget/textwidget")
local TitleBar = require("ui/widget/titlebar")
local TopContainer = require("ui/widget/container/topcontainer")
local UIManager = require("ui/uimanager")
local VerticalGroup = require("ui/widget/verticalgroup")
local VerticalSpan = require("ui/widget/verticalspan")
local Client = require("sig_client")
local Core = require("sig_core")
local Format = require("sig_format")
local _ = require("gettext")
local T = require("ffi/util").template
local Screen = Device.screen

local ChatList = InputContainer:extend{
    name = "signal_chats",
    covers_fullscreen = true,
    plugin = nil,
    archived = false,
    on_close = nil,
}

function ChatList:init()
    self.width = Screen:getWidth()
    self.height = Screen:getHeight()
    self.dimen = Geom:new{ x = 0, y = 0, w = self.width, h = self.height }
    self.page = 1
    self.chats = {}
    self.filter = nil
    self.name_face = Font:getFace("cfont", 20)
    self.name_face_bold = Font:getFace("cfont", 20)
    self.preview_face = Font:getFace("cfont", 16)
    self.small_face = Font:getFace("cfont", 14)
    self.row_h = math.floor(Screen:scaleBySize(78))
    if Device:hasKeys() then
        self.key_events = {
            Close = { { Device.input.group.Back } },
            NextPage = { { Device.input.group.PgFwd } },
            PrevPage = { { Device.input.group.PgBack } },
        }
    end
    self.ges_events = {
        TapList = { GestureRange:new{ ges = "tap", range = function() return self.list_dimen end } },
        HoldList = { GestureRange:new{ ges = "hold", range = function() return self.list_dimen end } },
        SwipeList = { GestureRange:new{ ges = "swipe", range = function() return self.list_dimen end } },
    }
    Core:addListener(self)
    self:load()
    self:build()
end

function ChatList:show()
    UIManager:show(self)
end

-- Data -----------------------------------------------------------------------------

function ChatList:load()
    local res, err = Client.get("/chats", { archived = self.archived and "1" or nil }, 5)
    if not res then
        self.load_error = err
        return
    end
    self.load_error = nil
    self.chats = res.chats or {}
    for _, c in ipairs(self.chats) do
        Core.chat_cache[c.id] = c
    end
    if not self.archived then
        local arch = Client.get("/chats", { archived = "1" }, 5)
        self.archived_unread = 0
        self.archived_count = arch and #(arch.chats or {}) or 0
        for _, c in ipairs(arch and arch.chats or {}) do
            Core.chat_cache[c.id] = c
            if not c.muted then self.archived_unread = self.archived_unread + (c.unread or 0) end
        end
    end
end

function ChatList:visibleChats()
    if not self.filter or self.filter == "" then return self.chats end
    local out = {}
    local f = self.filter:lower()
    for _, c in ipairs(self.chats) do
        if (c.name or ""):lower():find(f, 1, true) or (c.phone or ""):find(f, 1, true) then
            table.insert(out, c)
        end
    end
    return out
end

-- Layout ---------------------------------------------------------------------------

function ChatList:subtitle()
    local st = Core.status
    if st.syncing == "history" then
        return _("Transferring message history from your phone…")
    elseif st.state == "connecting" or st.state == "starting" then
        return _("Connecting…")
    elseif st.state == "disconnected" then
        return _("Offline — messages arrive when back online")
    elseif st.state == "helper_down" then
        return _("Signal helper not running")
    elseif st.state == "error" then
        return _("Connection problem — retrying")
    elseif st.state == "logged_out" then
        return _("This Kindle was unlinked")
    elseif st.syncing == "messages" then
        return _("Fetching new messages…")
    end
    if self.filter then return T(_("Search: %1"), self.filter) end
    return nil
end

function ChatList:build()
    local title = self.archived and _("Archived chats") or _("Signal")
    self.title_bar = TitleBar:new{
        width = self.width,
        fullscreen = true,
        with_bottom_line = true,
        title = title,
        subtitle = self:subtitle() or " ",
        left_icon = "appbar.settings",
        left_icon_tap_callback = function() self.plugin:showSettingsDialog(self) end,
        close_callback = function() self:onClose() end,
        show_parent = self,
    }
    local chats = self:visibleChats()
    local footer = self:buildFooter(chats)
    local list_h = self.height - self.title_bar:getSize().h - footer:getSize().h
    self.per_page = math.max(1, math.floor(list_h / self.row_h))
    self.pages = math.max(1, math.ceil(#chats / self.per_page))
    if self.page > self.pages then self.page = self.pages end
    -- the footer shows the page number, so build it again now that we know it
    footer = self:buildFooter(chats)
    local rows = VerticalGroup:new{ align = "left" }
    self.row_items = {}
    local first = (self.page - 1) * self.per_page + 1
    for i = first, math.min(#chats, first + self.per_page - 1) do
        table.insert(rows, self:buildRow(chats[i]))
        table.insert(self.row_items, chats[i])
    end
    if #chats == 0 then
        local msg
        if self.load_error then
            msg = _("Couldn't load chats: ") .. self.load_error
        elseif self.filter then
            msg = _("No chats match your search.")
        elseif self.archived then
            msg = _("No archived chats.")
        elseif Core.status.syncing == "history" then
            msg = _("Your chats will appear here once your phone finishes sending them.")
        else
            msg = _("No chats yet. New messages will appear here.\nTap “New chat” to write to someone.")
        end
        table.insert(rows, VerticalSpan:new{ width = Size.span.vertical_large * 4 })
        local TextBoxWidget = require("ui/widget/textboxwidget")
        table.insert(rows, FrameContainer:new{
            bordersize = 0,
            padding = Size.padding.large * 2,
            TextBoxWidget:new{
                text = msg,
                face = self.preview_face,
                width = self.width - 4 * Size.padding.large * 2,
                alignment = "center",
            },
        })
    end
    local list_area = FrameContainer:new{
        bordersize = 0,
        padding = 0,
        width = self.width,
        height = list_h,
        TopContainer:new{
            dimen = Geom:new{ w = self.width, h = list_h },
            rows,
        },
    }
    self.list_dimen = Geom:new{ x = 0, y = self.title_bar:getSize().h, w = self.width, h = list_h }
    self[1] = FrameContainer:new{
        width = self.width,
        height = self.height,
        background = Blitbuffer.COLOR_WHITE,
        bordersize = 0,
        padding = 0,
        VerticalGroup:new{
            align = "left",
            self.title_bar,
            list_area,
            footer,
        },
    }
end

function ChatList:buildRow(c)
    local pad = Size.padding.large
    local inner_w = self.width - 2 * pad
    local right_w = math.floor(inner_w * 0.22)
    local left_w = inner_w - right_w - pad
    local unread = (c.unread or 0) > 0
    local name = c.name or "?"
    if c.pinned then name = "★ " .. name end
    local name_w = TextWidget:new{
        text = name,
        face = self.name_face,
        bold = unread,
        max_width = left_w,
    }
    local preview = c.preview or ""
    if c.preview_mine and preview ~= "" then
        preview = _("You: ") .. preview
    elseif c.preview_from and c.preview_from ~= "" then
        preview = c.preview_from .. ": " .. preview
    end
    if c.request then preview = _("Message request") .. " · " .. preview end
    local preview_w = TextWidget:new{
        text = Format.oneLine(preview, 200),
        face = self.preview_face,
        fgcolor = unread and Blitbuffer.COLOR_BLACK or Blitbuffer.COLOR_DARK_GRAY,
        max_width = left_w,
    }
    local time_w = TextWidget:new{
        text = Format.listTime(c.last_ts),
        face = self.small_face,
        bold = unread,
    }
    local badge
    if unread then
        badge = FrameContainer:new{
            bordersize = Size.border.thin,
            radius = Screen:scaleBySize(12),
            background = c.muted and Blitbuffer.COLOR_WHITE or Blitbuffer.COLOR_BLACK,
            padding = 0,
            padding_left = Screen:scaleBySize(8),
            padding_right = Screen:scaleBySize(8),
            TextWidget:new{
                text = c.unread > 99 and "99+" or tostring(c.unread),
                face = self.small_face,
                bold = true,
                fgcolor = c.muted and Blitbuffer.COLOR_BLACK or Blitbuffer.COLOR_WHITE,
            },
        }
    elseif c.muted then
        badge = TextWidget:new{ text = _("muted"), face = self.small_face, fgcolor = Blitbuffer.COLOR_DARK_GRAY }
    else
        badge = VerticalSpan:new{ width = 0 }
    end
    local row_inner_h = self.row_h - Size.line.thin
    local left = VerticalGroup:new{
        align = "left",
        name_w,
        VerticalSpan:new{ width = Screen:scaleBySize(4) },
        preview_w,
    }
    local right = VerticalGroup:new{
        align = "right",
        time_w,
        VerticalSpan:new{ width = Screen:scaleBySize(6) },
        badge,
    }
    local row = OverlapGroup:new{
        dimen = Geom:new{ w = inner_w, h = row_inner_h },
        LeftContainer:new{ dimen = Geom:new{ w = left_w, h = row_inner_h }, left },
        RightContainer:new{ dimen = Geom:new{ w = inner_w, h = row_inner_h }, right },
    }
    return VerticalGroup:new{
        HorizontalGroup:new{ HorizontalSpan:new{ width = pad }, row, HorizontalSpan:new{ width = pad } },
        LineWidget:new{
            dimen = Geom:new{ w = self.width, h = Size.line.thin },
            background = Blitbuffer.COLOR_LIGHT_GRAY,
        },
    }
end

function ChatList:buildFooter(chats)
    local w = self.width
    local rows = {}
    local pages = self.pages or 1
    if pages > 1 then
        table.insert(rows, {
            { text = "◀", enabled = self.page > 1, callback = function() self:onPrevPage() end },
            { text = T(_("Page %1 of %2"), self.page, pages), enabled = false, callback = function() end },
            { text = "▶", enabled = self.page < pages, callback = function() self:onNextPage() end },
        })
    end
    local row = {}
    if not self.archived then
        table.insert(row, { text = _("New chat"), callback = function() self:newChat() end })
    end
    if self.filter then
        table.insert(row, { text = _("Clear search"), callback = function() self:setFilter(nil) end })
    else
        table.insert(row, { text = _("Search"), callback = function() self:search() end })
    end
    if self.archived then
        table.insert(row, { text = _("Back to chats"), callback = function() self:onClose() end })
    else
        local label = _("Archived")
        if (self.archived_unread or 0) > 0 then
            label = T(_("Archived (%1)"), self.archived_unread)
        end
        table.insert(row, {
            text = label,
            enabled = (self.archived_count or 0) > 0,
            callback = function() self:showArchived() end,
        })
    end
    table.insert(rows, row)
    return FrameContainer:new{
        bordersize = 0,
        padding = 0,
        padding_bottom = Size.padding.small,
        VerticalGroup:new{
            LineWidget:new{ dimen = Geom:new{ w = w, h = Size.line.medium }, background = Blitbuffer.COLOR_DARK_GRAY },
            ButtonTable:new{
                width = w,
                buttons = rows,
                zero_sep = true,
                show_parent = self,
            },
        },
    }
end

function ChatList:refresh(mode)
    if self.closed then return end
    self:build()
    UIManager:setDirty(self, mode or "ui")
end

function ChatList:reload(mode)
    self:load()
    self:refresh(mode)
end

-- Gestures -------------------------------------------------------------------------

function ChatList:itemAt(pos)
    if not pos or not self.list_dimen then return end
    local idx = math.floor((pos.y - self.list_dimen.y) / self.row_h) + 1
    return self.row_items and self.row_items[idx]
end

function ChatList:onTapList(_, ges)
    local c = self:itemAt(ges.pos)
    if c then self:openChat(c) end
    return true
end

function ChatList:onHoldList(_, ges)
    local c = self:itemAt(ges.pos)
    if c then self:chatOptions(c) end
    return true
end

function ChatList:onSwipeList(_, ges)
    if ges.direction == "west" or ges.direction == "north" then
        self:onNextPage()
    elseif ges.direction == "east" or ges.direction == "south" then
        self:onPrevPage()
    end
    return true
end

function ChatList:onNextPage()
    if self.page < (self.pages or 1) then
        self.page = self.page + 1
        self:refresh("ui")
    end
    return true
end

function ChatList:onPrevPage()
    if self.page > 1 then
        self.page = self.page - 1
        self:refresh("ui")
    end
    return true
end

-- Actions --------------------------------------------------------------------------

function ChatList:openChat(c)
    local ChatView = require("sig_chat")
    local view = ChatView:new{ chat = c, plugin = self.plugin }
    view.on_close = function()
        self.child = nil
        self:reload("ui")
    end
    self.child = view
    UIManager:show(view)
end

function ChatList:openChatById(chat_id)
    local c = Client.get("/chat", { chat = chat_id }, 5)
    if c then
        Core.chat_cache[c.id] = c
        self:openChat(c)
    end
end

function ChatList:setFlag(c, flag, value)
    local res, err = Client.post("/chat-flag", { chat = c.id, [flag] = value }, 5)
    if not res then
        UIManager:show(InfoMessage:new{ text = err })
    end
    self:reload("ui")
end

function ChatList:chatOptions(c)
    local dialog
    local buttons = {
        { {
            text = c.pinned and _("Unpin") or _("Pin to top"),
            callback = function() UIManager:close(dialog) self:setFlag(c, "pinned", not c.pinned) end,
        } },
        { {
            text = c.muted and _("Unmute notifications") or _("Mute notifications"),
            callback = function() UIManager:close(dialog) self:setFlag(c, "muted", not c.muted) end,
        } },
        { {
            text = c.archived and _("Move back to chats") or _("Archive"),
            callback = function() UIManager:close(dialog) self:setFlag(c, "archived", not c.archived) end,
        } },
        { {
            text = _("Clear messages on this Kindle"),
            callback = function()
                UIManager:close(dialog)
                UIManager:show(ConfirmBox:new{
                    text = T(_("Remove the messages of “%1” from this Kindle?\n\nYour phone keeps them."), c.name),
                    ok_text = _("Clear"),
                    ok_callback = function()
                        Client.post("/clear-chat", { chat = c.id }, 5)
                        self:reload("ui")
                    end,
                })
            end,
        } },
    }
    dialog = ButtonDialog:new{
        title = c.name,
        title_align = "center",
        buttons = buttons,
    }
    UIManager:show(dialog)
end

function ChatList:search()
    local dialog
    dialog = InputDialog:new{
        title = _("Search chats"),
        input = self.filter or "",
        input_hint = _("Name or number"),
        buttons = { {
            { text = _("Cancel"), id = "close", callback = function() UIManager:close(dialog) end },
            {
                text = _("Search"),
                is_enter_default = true,
                callback = function()
                    local q = dialog:getInputText()
                    UIManager:close(dialog)
                    self:setFilter(q ~= "" and q or nil)
                end,
            },
        } },
    }
    UIManager:show(dialog)
    dialog:onShowKeyboard()
end

function ChatList:setFilter(q)
    self.filter = q
    self.page = 1
    self:refresh("ui")
end

function ChatList:showArchived()
    local list = ChatList:new{ plugin = self.plugin, archived = true }
    list.on_close = function() self:reload("ui") end
    UIManager:show(list)
end

function ChatList:newChat()
    local res, err = Client.get("/contacts", nil, 8)
    if not res then
        UIManager:show(InfoMessage:new{ text = err })
        return
    end
    local menu
    local items = {
        { text = _("Find by phone number…"), bold = true, callback = function() self:findBy("phone") end },
        { text = _("Find by username…"), bold = true, callback = function() self:findBy("username") end },
    }
    for _, ct in ipairs(res.contacts or {}) do
        table.insert(items, {
            text = ct.name,
            mandatory = ct.phone,
            callback = function()
                UIManager:close(menu)
                self:startChat(ct.id)
            end,
        })
    end
    menu = Menu:new{
        title = _("New chat"),
        item_table = items,
        is_borderless = true,
        is_popout = false,
        is_enable_shortcut = false,
        covers_fullscreen = true,
        width = Screen:getWidth(),
        height = Screen:getHeight(),
        onMenuSelect = function(m, item)
            if item.callback then item.callback() end
        end,
    }
    self.new_chat_menu = menu
    UIManager:show(menu)
end

function ChatList:startChat(chat_id)
    local res, err = Client.post("/open", { chat = chat_id }, 10)
    if not res then
        UIManager:show(InfoMessage:new{ text = err })
        return
    end
    Core.chat_cache[res.id] = res
    self:openChat(res)
end

function ChatList:findBy(kind)
    local dialog
    dialog = InputDialog:new{
        title = kind == "phone" and _("Phone number") or _("Signal username"),
        input_hint = kind == "phone" and "+351 912 345 678" or "name.42",
        input_type = kind == "phone" and "phone" or nil,
        buttons = { {
            { text = _("Cancel"), id = "close", callback = function() UIManager:close(dialog) end },
            {
                text = _("Find"),
                is_enter_default = true,
                callback = function()
                    local q = dialog:getInputText()
                    UIManager:close(dialog)
                    local wait = InfoMessage:new{ text = _("Looking up…") }
                    UIManager:show(wait)
                    UIManager:forceRePaint()
                    local res, err = Client.post("/lookup", { [kind] = q }, 30)
                    UIManager:close(wait)
                    if not res then
                        UIManager:show(InfoMessage:new{ text = err })
                        return
                    end
                    if self.new_chat_menu then
                        UIManager:close(self.new_chat_menu)
                        self.new_chat_menu = nil
                    end
                    self:startChat(res.chat)
                end,
            },
        } },
    }
    UIManager:show(dialog)
    dialog:onShowKeyboard()
end

-- Live updates ---------------------------------------------------------------------

function ChatList:onSignalEvent(ev)
    if self.closed then return end
    if ev.type == "status" then
        if ev.status.state == "logged_out" and ev.old.state ~= "logged_out" and not self.archived then
            self.plugin:offerRelink(self)
        end
        if not self.child then self:refresh("ui") end
        return
    end
    if ev.type == "msg" or ev.type == "chat" or ev.type == "chats" or ev.type == "names"
        or ev.type == "remove" or ev.type == "reload" then
        if self.child then return end -- reloaded when the chat closes
        if not self._reload_pending then
            self._reload_pending = true
            UIManager:scheduleIn(0.6, function()
                self._reload_pending = false
                if not self.closed and not self.child then self:reload("ui") end
            end)
        end
    end
end

function ChatList:onClose()
    if self.closed then return true end
    self.closed = true
    Core:removeListener(self)
    UIManager:close(self)
    if self.on_close then self.on_close() end
    return true
end

return ChatList
