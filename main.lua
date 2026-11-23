--[[--
Signal for KOReader.

Links to your Signal account as a linked device (like Signal Desktop) by
scanning a QR code with your phone. The Signal connection and encryption run
in a small helper program (bin/) that this plugin starts and talks to over
localhost.

@module koplugin.signal
]]

local ConfirmBox = require("ui/widget/confirmbox")
local DataStorage = require("datastorage")
local Device = require("device")
local Dispatcher = require("dispatcher")
local InfoMessage = require("ui/widget/infomessage")
local InputDialog = require("ui/widget/inputdialog")
local MultiInputDialog = require("ui/widget/multiinputdialog")
local NetworkMgr = require("ui/network/manager")
local TextViewer = require("ui/widget/textviewer")
local UIManager = require("ui/uimanager")
local WidgetContainer = require("ui/widget/container/widgetcontainer")
local util = require("util")
local Client = require("sig_client")
local Core = require("sig_core")
local _ = require("gettext")
local T = require("ffi/util").template

local Signal = WidgetContainer:extend{
    name = "signal",
    is_doc_only = false,
}

function Signal:init()
    Core:init(self.path)
    Core.plugin = self
    self:onDispatcherRegisterActions()
    self.ui.menu:registerToMainMenu(self)
    if not Core.autostarted then
        Core.autostarted = true
        if Core:get("linked_once", false) and Core:wantsBackground() then
            -- not during startup: let KOReader finish opening first
            UIManager:scheduleIn(15, function()
                Core:ensureHelper(function(ok) if ok then Core:startPolling() end end)
            end)
        end
    end
end

function Signal:onDispatcherRegisterActions()
    Dispatcher:registerAction("signal_open", {
        category = "none",
        event = "OpenSignal",
        title = _("Open Signal"),
        general = true,
    })
end

function Signal:addToMainMenu(menu_items)
    menu_items.signal = {
        text = _("Signal"),
        sorting_hint = "tools",
        sub_item_table_func = function() return self:menuItems() end,
    }
end

-- Opening ------------------------------------------------------------------------

function Signal:onOpenSignal()
    self:open()
    return true
end

function Signal:withHelper(callback)
    local starting
    local st = Core.status.state
    if st == "helper_down" or st == "stopped" or st == "starting" then
        starting = InfoMessage:new{ text = _("Starting Signal…") }
        UIManager:show(starting)
        UIManager:forceRePaint()
    end
    Core:ensureHelper(function(ok, err)
        if starting then UIManager:close(starting) end
        if not ok then
            UIManager:show(InfoMessage:new{ text = err or _("The Signal helper is not running.") })
            return
        end
        Core:startPolling()
        callback()
    end)
end

function Signal:open()
    local function go()
        self:withHelper(function()
            if Core:isLinked() then
                self:showChats()
            else
                self:link()
            end
        end)
    end
    if Core:isRemote() or Core:isLinked() then
        go()
    else
        -- linking needs the network; KOReader offers to turn Wi-Fi on
        NetworkMgr:runWhenOnline(go)
    end
end

function Signal:showChats()
    if Core.chat_list and not Core.chat_list.closed then return end
    local ChatList = require("sig_chatlist")
    local list = ChatList:new{ plugin = self }
    list.on_close = function()
        if Core.chat_list == list then Core.chat_list = nil end
        self:afterClose()
    end
    Core.chat_list = list
    list:show()
end

function Signal:link()
    local QRLogin = require("sig_qrlogin")
    local screen = QRLogin:new{
        on_linked = function()
            Core:set("linked_once", true)
            self:showChats()
        end,
    }
    UIManager:show(screen)
end

--- When "stay connected" is off, stop the helper once Signal is closed.
function Signal:afterClose()
    if not Core:wantsBackground() and not Core:isRemote() then
        UIManager:scheduleIn(1, function()
            if not Core.chat_list then Core:stopHelper() end
        end)
    end
end

-- Files ----------------------------------------------------------------------------

function Signal:downloadDir()
    local home = G_reader_settings:readSetting("home_dir") or Device.home_dir or DataStorage:getDataDir()
    local dir = Core:get("download_dir") or (home .. "/Signal")
    util.makePath(dir)
    return dir
end

function Signal:downloadPath(md, m)
    local name = md.name
    if not name or name == "" then
        local ext = (md.mime or ""):match("/([%w%-%.]+)$") or "bin"
        ext = ext:gsub("^x%-", ""):gsub("jpeg", "jpg"):gsub("mpeg", "mp3"):gsub("plain", "txt")
        name = os.date("signal-%Y%m%d-%H%M%S", math.floor(m.ts / 1000)) .. "." .. ext
    end
    name = name:gsub("[/\\:*?\"<>|]", "_")
    local dir = self:downloadDir()
    local path = dir .. "/" .. name
    local base, ext = name:match("^(.*)(%.[^.]+)$")
    base = base or name
    ext = ext or ""
    local n = 1
    while util.fileExists(path) do
        n = n + 1
        path = string.format("%s/%s (%d)%s", dir, base, n, ext)
    end
    return path
end

function Signal:offerOpen(path)
    local DocumentRegistry = require("document/documentregistry")
    if DocumentRegistry:hasProvider(path) then
        UIManager:show(ConfirmBox:new{
            text = T(_("Saved to %1\n\nOpen it now?"), path),
            ok_text = _("Open"),
            ok_callback = function()
                if Core.chat_list then Core.chat_list:onClose() end
                for _, w in ipairs(UIManager._window_stack) do
                    if w.widget.name == "signal_chat" then w.widget:onClose() end
                end
                local ReaderUI = require("apps/reader/readerui")
                ReaderUI:showReader(path)
            end,
        })
    else
        UIManager:show(InfoMessage:new{ text = T(_("Saved to %1"), path) })
    end
end

-- Menus ------------------------------------------------------------------------------

function Signal:menuItems()
    return {
        {
            text = _("Open Signal"),
            callback = function() self:open() end,
            separator = true,
        },
        {
            text = _("Stay connected while reading"),
            help_text = _("Keeps Signal running in the background so notifications arrive while you read. Uses more battery."),
            checked_func = function() return Core:wantsBackground() end,
            callback = function()
                Core:set("background", not Core:wantsBackground())
                if Core:wantsBackground() then
                    Core:ensureHelper(function(ok) if ok then Core:startPolling() end end)
                elseif not Core.chat_list then
                    Core:stopHelper()
                end
            end,
        },
        {
            text = _("Notifications"),
            checked_func = function() return Core:get("notifications", true) end,
            callback = function() Core:set("notifications", not Core:get("notifications", true)) end,
        },
        {
            text = _("Show message text in notifications"),
            enabled_func = function() return Core:get("notifications", true) end,
            checked_func = function() return Core:get("notification_preview", true) end,
            callback = function() Core:set("notification_preview", not Core:get("notification_preview", true)) end,
        },
        {
            text = _("Show photos in chats"),
            help_text = _("Downloads photos automatically and shows them inside the chat."),
            checked_func = function() return Core:get("show_photos", true) end,
            callback = function() Core:set("show_photos", not Core:get("show_photos", true)) end,
        },
        {
            text_func = function() return T(_("Text size: %1"), Core:get("font_size", 19)) end,
            keep_menu_open = true,
            callback = function(touchmenu_instance)
                local SpinWidget = require("ui/widget/spinwidget")
                UIManager:show(SpinWidget:new{
                    title_text = _("Message text size"),
                    value = Core:get("font_size", 19),
                    value_min = 14,
                    value_max = 30,
                    default_value = 19,
                    callback = function(spin)
                        Core:set("font_size", spin.value)
                        if touchmenu_instance then touchmenu_instance:updateItems() end
                    end,
                })
            end,
            separator = true,
        },
        {
            text = _("Advanced"),
            sub_item_table = self:advancedItems(),
        },
        {
            text = _("Unlink this Kindle"),
            enabled_func = function() return Core:isLinked() end,
            callback = function() self:confirmLogout() end,
        },
    }
end

function Signal:advancedItems()
    return {
        {
            text = _("Helper log"),
            callback = function() self:showLog() end,
        },
        {
            text = _("Restart the helper"),
            enabled_func = function() return not Core:isRemote() end,
            callback = function()
                Core:stopHelper()
                UIManager:scheduleIn(1.5, function()
                    Core:ensureHelper(function(ok, err)
                        if ok then Core:startPolling() end
                        UIManager:show(InfoMessage:new{ text = ok and _("Signal helper restarted.") or err, timeout = 3 })
                    end)
                end)
            end,
        },
        {
            text = _("Reconnect now"),
            callback = function()
                Client.post("/reconnect", {}, 3)
                UIManager:show(InfoMessage:new{ text = _("Reconnecting…"), timeout = 2 })
            end,
        },
        {
            text_func = function()
                return T(_("Device name: %1"), Core:get("device_name", "Kindle (KOReader)"))
            end,
            help_text = _("The name shown in Signal → Linked devices. Takes effect the next time you link."),
            keep_menu_open = true,
            callback = function(touchmenu_instance)
                local dialog
                dialog = InputDialog:new{
                    title = _("Device name"),
                    input = Core:get("device_name", "Kindle (KOReader)"),
                    buttons = { {
                        { text = _("Cancel"), id = "close", callback = function() UIManager:close(dialog) end },
                        { text = _("Save"), is_enter_default = true, callback = function()
                            local v = dialog:getInputText()
                            UIManager:close(dialog)
                            if v ~= "" then Core:set("device_name", v) end
                            if touchmenu_instance then touchmenu_instance:updateItems() end
                        end },
                    } },
                }
                UIManager:show(dialog)
                dialog:onShowKeyboard()
            end,
        },
        {
            text = _("Use a helper on another computer"),
            help_text = _("Run sig-helper on a computer that's always on (e.g. a Raspberry Pi) and let the Kindle connect to it. Leave the address empty to use the helper on this Kindle."),
            checked_func = function() return Core:isRemote() end,
            callback = function() self:remoteDialog() end,
        },
        {
            text = _("About"),
            callback = function()
                local st = Core.status
                local me = st.me and (st.me.number or st.me.aci) or _("not linked")
                UIManager:show(InfoMessage:new{
                    text = T(_("Signal for KOReader\nHelper %1 — %2\nAccount: %3\nState: %4\n\nUses mautrix-signal's signalmeow and Signal's libsignal (AGPL-3.0)."),
                        st.version or "?", Client.base, me, st.state or "?"),
                })
            end,
        },
    }
end

function Signal:remoteDialog()
    local dialog
    dialog = MultiInputDialog:new{
        title = _("Helper on another computer"),
        fields = {
            { text = Core:get("remote_url", ""), hint = "http://192.168.1.20:18782", description = _("Address") },
            { text = Core:get("remote_token", ""), hint = _("token"), description = _("Token (-token)"), text_type = "password" },
        },
        buttons = { {
            { text = _("Cancel"), id = "close", callback = function() UIManager:close(dialog) end },
            { text = _("Save"), callback = function()
                local f = dialog:getFields()
                UIManager:close(dialog)
                Core:stopPolling()
                Core:set("remote_url", f[1])
                Core:set("remote_token", f[2])
                Core:applyConnection()
                Core:ensureHelper(function(ok, err)
                    if ok then
                        Core:startPolling()
                        UIManager:show(InfoMessage:new{ text = _("Connected to the helper."), timeout = 2 })
                    else
                        UIManager:show(InfoMessage:new{ text = err })
                    end
                end)
            end },
        } },
    }
    UIManager:show(dialog)
    dialog:onShowKeyboard()
end

function Signal:showLog()
    local f = io.open(Core:logPath(), "r")
    local text = _("(no log yet)")
    if f then
        local size = f:seek("end")
        f:seek("set", math.max(0, size - 24000))
        text = f:read("*a")
        f:close()
    end
    UIManager:show(TextViewer:new{ title = _("Signal helper log"), text = text, monospace_font = true })
end

function Signal:confirmLogout()
    UIManager:show(ConfirmBox:new{
        text = _("Unlink this Kindle from your Signal account?\n\nMessages stored on the Kindle are deleted. Your phone keeps everything."),
        ok_text = _("Unlink"),
        ok_callback = function()
            local res, err = Client.post("/logout", {}, 20)
            if not res then
                UIManager:show(InfoMessage:new{ text = err })
                return
            end
            Core:set("linked_once", false)
            Core:refreshStatus()
            if Core.chat_list then Core.chat_list:onClose() end
            UIManager:show(InfoMessage:new{ text = _("This Kindle is no longer linked."), timeout = 3 })
        end,
    })
end

function Signal:offerRelink(list)
    UIManager:show(ConfirmBox:new{
        text = _("Signal unlinked this Kindle (it was removed on the phone, or was offline for too long).\n\nLink it again?"),
        ok_text = _("Link again"),
        ok_callback = function()
            list:onClose()
            self:link()
        end,
    })
end

--- Settings shortcut from the chat list title bar.
function Signal:showSettingsDialog(list)
    local ButtonDialog = require("ui/widget/buttondialog")
    local dialog
    local function toggle(key, default)
        return function()
            Core:set(key, not Core:get(key, default))
            UIManager:close(dialog)
            self:showSettingsDialog(list)
        end
    end
    local function mark(key, default)
        return Core:get(key, default) and "☑ " or "☐ "
    end
    dialog = ButtonDialog:new{
        title = _("Signal settings"),
        title_align = "center",
        buttons = {
            { { text = mark("background", true) .. _("Stay connected while reading"), align = "left", callback = toggle("background", true) } },
            { { text = mark("notifications", true) .. _("Notifications"), align = "left", callback = toggle("notifications", true) } },
            { { text = mark("show_photos", true) .. _("Show photos in chats"), align = "left", callback = toggle("show_photos", true) } },
            { { text = _("Helper log"), callback = function() UIManager:close(dialog) self:showLog() end } },
            { { text = _("Unlink this Kindle"), enabled = Core:isLinked(), callback = function()
                UIManager:close(dialog)
                self:confirmLogout()
            end } },
        },
    }
    UIManager:show(dialog)
end

--- The ⋮ menu inside a chat.
function Signal:chatMenu(view)
    local ButtonDialog = require("ui/widget/buttondialog")
    local c = view.chat
    local dialog
    local function flag(name, value)
        UIManager:close(dialog)
        Client.post("/chat-flag", { chat = c.id, [name] = value }, 5)
        view:refreshChatInfo()
        view:render("ui")
    end
    dialog = ButtonDialog:new{
        title = c.name,
        title_align = "center",
        buttons = {
            { { text = _("Refresh"), callback = function()
                UIManager:close(dialog)
                view:refreshChatInfo()
                view:loadInitial()
                view:render("full")
            end } },
            { { text = c.muted and _("Unmute notifications") or _("Mute notifications"), callback = function() flag("muted", not c.muted) end } },
            { { text = c.pinned and _("Unpin") or _("Pin to top"), callback = function() flag("pinned", not c.pinned) end } },
            { { text = c.archived and _("Move back to chats") or _("Archive"), callback = function() flag("archived", not c.archived) end } },
            { { text = _("Clear messages on this Kindle"), callback = function()
                UIManager:close(dialog)
                UIManager:show(ConfirmBox:new{
                    text = _("Remove this chat's messages from the Kindle? Your phone keeps them."),
                    ok_text = _("Clear"),
                    ok_callback = function()
                        Client.post("/clear-chat", { chat = c.id }, 5)
                        view:loadInitial()
                        view:render("ui")
                    end,
                })
            end } },
        },
    }
    UIManager:show(dialog)
end

-- System events --------------------------------------------------------------------

function Signal:onSuspend()
    Core.suspended = true
end

function Signal:onResume()
    Core.suspended = false
    if Core.polling then
        -- the Wi-Fi may need a moment; the helper reconnects by itself
        UIManager:scheduleIn(3, function() Core:poll() end)
    end
end

function Signal:onNetworkConnected()
    if Core.polling and (Core.status.state == "disconnected" or Core.status.state == "error") then
        Client.post("/reconnect", {}, 3)
    end
end

return Signal
