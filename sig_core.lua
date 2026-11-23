--[[--
Shared state: the helper process, its status, the event poller and
notifications. Screens register as listeners to get live updates.

@module koplugin.signal.core
]]

local DataStorage = require("datastorage")
local LuaSettings = require("luasettings")
local UIManager = require("ui/uimanager")
local ffi = require("ffi")
local logger = require("logger")
local util = require("util")
local Client = require("sig_client")
local _ = require("gettext")
local T = require("ffi/util").template

pcall(ffi.cdef, "int getpid(void);")

local Core = {
    status = { state = "stopped" },
    seq = 0,
    listeners = {},
    polling = false,
    fast = false,
    open_chat = nil, -- chat id currently on screen
    pending_notes = {},
}

local DEFAULT_PORT = 18782

function Core:init(plugin_path)
    if self.initialized then return end
    self.initialized = true
    self.plugin_path = plugin_path
    self.data_dir = DataStorage:getSettingsDir() .. "/signal"
    util.makePath(self.data_dir)
    self.settings = LuaSettings:open(DataStorage:getSettingsDir() .. "/signal.lua")
    local token = self.settings:readSetting("token")
    if not token then
        local chars = {}
        math.randomseed(os.time() + (tonumber(tostring({}):match("0x(%x+)") or "0", 16) or 0))
        for i = 1, 32 do
            local n = math.random(0, 61)
            chars[i] = string.char(n < 10 and 48 + n or (n < 36 and 55 + n or 61 + n))
        end
        token = table.concat(chars)
        self.settings:saveSetting("token", token)
        self.settings:flush()
    end
    self:applyConnection()
end

function Core:get(key, default)
    local v = self.settings:readSetting(key)
    if v == nil then return default end
    return v
end

function Core:set(key, value)
    self.settings:saveSetting(key, value)
    self.settings:flush()
end

function Core:isRemote()
    return (self:get("remote_url", "") or "") ~= ""
end

function Core:applyConnection()
    if self:isRemote() then
        Client.base = self:get("remote_url"):gsub("/+$", "")
        Client.token = self:get("remote_token", "")
    else
        Client.base = "http://127.0.0.1:" .. self:get("port", DEFAULT_PORT)
        Client.token = self:get("token")
    end
end

function Core:wantsBackground()
    return self:get("background", true)
end

function Core:isLinked()
    return self.status.linked == true
end

-- Listeners ------------------------------------------------------------------------

function Core:addListener(l)
    for _, v in ipairs(self.listeners) do
        if v == l then return end
    end
    table.insert(self.listeners, l)
    self:updatePollRate()
end

function Core:removeListener(l)
    for i, v in ipairs(self.listeners) do
        if v == l then
            table.remove(self.listeners, i)
            break
        end
    end
    self:updatePollRate()
end

function Core:emit(ev)
    for _, l in ipairs({ unpack(self.listeners) }) do
        if l.onSignalEvent then
            local ok, err = pcall(l.onSignalEvent, l, ev)
            if not ok then logger.warn("signal: listener error", err) end
        end
    end
end

function Core:setStatus(st)
    local old = self.status
    self.status = st
    if old.state ~= st.state or old.qr ~= st.qr or old.syncing ~= st.syncing or old.linked ~= st.linked or old.detail ~= st.detail then
        self:emit({ type = "status", status = st, old = old })
    end
end

function Core:refreshStatus()
    local st, err = Client.get("/status", nil, 3)
    if st then
        self:setStatus(st)
    else
        self:setStatus({ state = "helper_down", detail = err })
    end
    return st, err
end

-- Helper process -------------------------------------------------------------------

local function arch()
    if Core._arch then return Core._arch end
    local p = io.popen("uname -m 2>/dev/null")
    local a = p and p:read("*l") or ""
    if p then p:close() end
    Core._arch = a
    return a
end

function Core:helperPath()
    local a = arch()
    local bin = self.plugin_path .. "/bin/"
    local candidates
    if a:match("^arm") or a == "aarch64" then
        candidates = { "sig-helper-armhf" }
    elseif a == "x86_64" then
        candidates = { "sig-helper-x86_64", "sig-helper-amd64" }
    else
        candidates = { "sig-helper-" .. a }
    end
    for _, name in ipairs(candidates) do
        local path = bin .. name
        if util.fileExists(path) then return path end
    end
    return nil, T(_("No helper program for this device (%1) in the plugin's bin folder."), a)
end

function Core:logPath()
    return self.data_dir .. "/helper.log"
end

function Core:spawnHelper()
    local path, err = self:helperPath()
    if not path then return nil, err end
    os.execute("chmod +x " .. util.shell_escape({ path }) .. " 2>/dev/null")
    -- keep the log short
    local log = self:logPath()
    local lf = io.open(log, "r")
    if lf then
        local size = lf:seek("end")
        lf:close()
        if size and size > 512 * 1024 then
            os.rename(log, log .. ".old")
        end
    end
    local pid = ffi.C.getpid()
    local cmd = string.format("%s -data %s -listen 127.0.0.1:%d -token %s -parent-pid %d -device-name %s >> %s 2>&1 &",
        util.shell_escape({ path }), util.shell_escape({ self.data_dir }), self:get("port", DEFAULT_PORT),
        util.shell_escape({ self:get("token") }), pid,
        util.shell_escape({ self:get("device_name", "Kindle (KOReader)") }), util.shell_escape({ log }))
    logger.info("signal: starting helper", path)
    os.execute(cmd)
    self.spawned_at = os.time()
    return true
end

--- Makes sure the helper answers; calls callback(ok, err) (maybe later).
function Core:ensureHelper(callback)
    self:applyConnection()
    local st = Client.get("/status", nil, 2)
    if st then
        self:setStatus(st)
        return callback(true)
    end
    if self:isRemote() then
        self:setStatus({ state = "helper_down" })
        return callback(false, T(_("Can't reach the Signal helper at %1."), Client.base))
    end
    if self._starting then
        table.insert(self._starting, callback)
        return
    end
    local ok, err = self:spawnHelper()
    if not ok then
        self:setStatus({ state = "helper_down", detail = err })
        return callback(false, err)
    end
    self._starting = { callback }
    self:setStatus({ state = "starting" })
    local tries = 0
    local function check()
        tries = tries + 1
        local s = Client.get("/status", nil, 1)
        if s then
            local cbs = self._starting
            self._starting = nil
            self:setStatus(s)
            for _, cb in ipairs(cbs) do cb(true) end
            return
        end
        if tries > 60 then
            local cbs = self._starting
            self._starting = nil
            local msg = _("The Signal helper didn't start. See its log in Settings.")
            self:setStatus({ state = "helper_down", detail = msg })
            for _, cb in ipairs(cbs) do cb(false, msg) end
            return
        end
        UIManager:scheduleIn(0.5, check)
    end
    UIManager:scheduleIn(0.3, check)
end

function Core:stopHelper()
    self:stopPolling()
    if not self:isRemote() then
        Client.post("/shutdown", {}, 2)
    end
    self:setStatus({ state = "stopped" })
end

-- Polling --------------------------------------------------------------------------

function Core:interval()
    if self.status.state == "pairing" or self.status.state == "linking" then return 1 end
    if #self.listeners > 0 then return 1.5 end
    return self:get("background_interval", 10)
end

function Core:updatePollRate()
    if self.polling then
        UIManager:unschedule(self._poll_fn)
        UIManager:scheduleIn(0.2, self._poll_fn)
    end
end

function Core:startPolling()
    if not self._poll_fn then
        self._poll_fn = function() self:poll() end
    end
    if self.polling then return end
    self.polling = true
    self:refreshStatus()
    UIManager:scheduleIn(0.2, self._poll_fn)
end

function Core:stopPolling()
    self.polling = false
    if self._poll_fn then UIManager:unschedule(self._poll_fn) end
end

function Core:poll()
    if not self.polling then return end
    if self.suspended then return end
    local res, err, code = Client.get("/events", { since = self.seq, wait = 0 }, 3)
    if not res then
        if self.status.state ~= "helper_down" then
            logger.warn("signal: poll failed", err)
            self:refreshStatus()
        end
        if self.status.state == "helper_down" and not self:isRemote() and (#self.listeners > 0 or self:wantsBackground()) then
            -- the helper went away (crash, or killed): start it again
            if not self.spawned_at or os.time() - self.spawned_at > 30 then
                self:ensureHelper(function() end)
            end
        end
    else
        if res.reset then
            self.seq = res.seq or 0
            self:refreshStatus()
            self:emit({ type = "reload" })
        else
            local status_changed = false
            local new_msgs = {}
            for _, ev in ipairs(res.events or {}) do
                self.seq = math.max(self.seq, ev.seq or 0)
                if ev.type == "state" or ev.type == "qr" then
                    status_changed = true
                end
                if ev.type == "msg" and ev.new and ev.msg then
                    table.insert(new_msgs, ev)
                end
            end
            if status_changed then self:refreshStatus() end
            for _, ev in ipairs(res.events or {}) do
                self:emit(ev)
            end
            if #new_msgs > 0 then self:notify(new_msgs) end
            if res.seq and res.seq > self.seq then self.seq = res.seq end
        end
    end
    if self.polling then
        UIManager:scheduleIn(self:interval(), self._poll_fn)
    end
end

-- Notifications --------------------------------------------------------------------

--- Chat details (name, muted…) by id, fetched once and kept up to date by the chat list.
Core.chat_cache = {}

function Core:chatInfo(chat_id)
    local c = self.chat_cache[chat_id]
    if not c then
        c = Client.get("/chat", { chat = chat_id }, 3)
        if c then self.chat_cache[chat_id] = c end
    end
    return c
end

function Core:chatMuted(chat_id)
    local c = self:chatInfo(chat_id)
    return c and c.muted
end

function Core:notify(evs)
    if not self:get("notifications", true) then return end
    local shown = {}
    for _, ev in ipairs(evs) do
        if ev.chat ~= self.open_chat and not self:chatMuted(ev.chat) and not ev.msg.from_me then
            table.insert(shown, ev)
        end
    end
    if #shown == 0 then return end
    local Notification = require("ui/widget/notification")
    local Format = require("sig_format")
    local text
    if #shown > 2 then
        local chats = {}
        local n = 0
        for _, ev in ipairs(shown) do
            if not chats[ev.chat] then
                chats[ev.chat] = true
                n = n + 1
            end
        end
        if n == 1 then
            text = T(_("Signal: %1 new messages from %2"), #shown, Format.chatTitle(shown[1].chat, shown[1].msg))
        else
            text = T(_("Signal: %1 new messages in %2 chats"), #shown, n)
        end
    else
        local ev = shown[#shown]
        local who = Format.chatTitle(ev.chat, ev.msg)
        if self:get("notification_preview", true) then
            text = who .. ": " .. Format.oneLine(Format.preview(ev.msg), 80)
        else
            text = T(_("Signal: new message from %1"), who)
        end
    end
    UIManager:show(Notification:new{ text = text, timeout = 4 })
end

return Core
