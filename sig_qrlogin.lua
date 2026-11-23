--[[--
Link screen: shows Signal's "link a device" QR code, refreshed as Signal
rotates it, and follows the linking until the chats are ready.

@module koplugin.signal.qrlogin
]]

local Blitbuffer = require("ffi/blitbuffer")
local ButtonTable = require("ui/widget/buttontable")
local CenterContainer = require("ui/widget/container/centercontainer")
local Device = require("device")
local Font = require("ui/font")
local FrameContainer = require("ui/widget/container/framecontainer")
local Geom = require("ui/geometry")
local InputContainer = require("ui/widget/container/inputcontainer")
local QRWidget = require("ui/widget/qrwidget")
local Size = require("ui/size")
local TextBoxWidget = require("ui/widget/textboxwidget")
local TitleBar = require("ui/widget/titlebar")
local UIManager = require("ui/uimanager")
local VerticalGroup = require("ui/widget/verticalgroup")
local VerticalSpan = require("ui/widget/verticalspan")
local Client = require("sig_client")
local Core = require("sig_core")
local _ = require("gettext")
local Screen = Device.screen

local QRLogin = InputContainer:extend{
    name = "signal_link",
    covers_fullscreen = true,
    on_linked = nil,
}

function QRLogin:init()
    self.width = Screen:getWidth()
    self.height = Screen:getHeight()
    self.dimen = Geom:new{ x = 0, y = 0, w = self.width, h = self.height }
    if Device:hasKeys() then
        self.key_events = { Close = { { Device.input.group.Back } } }
    end
    self.shown_qr = nil
    Core:addListener(self)
    local st = Core.status
    if not (st.state == "pairing" and st.qr) then
        self:requestCode()
    end
    self:build()
end

function QRLogin:requestCode()
    local res, err = Client.post("/login", {}, 10)
    if not res then
        Core:setStatus({ state = "unpaired", detail = err })
    else
        Core:refreshStatus()
    end
end

function QRLogin:statusLine()
    local st = Core.status
    if st.state == "unpaired" and st.detail == "expired" then
        return _("The code expired. Tap “New code” to get another one.")
    elseif st.state == "unpaired" and st.detail == "too_many_devices" then
        return _("Your account already has the most linked devices Signal allows. Unlink one on your phone, then try again.")
    elseif st.state == "unpaired" and st.detail and st.detail ~= "" then
        return _("Couldn't get a code: ") .. st.detail
    elseif st.state == "logged_out" then
        return _("This Kindle was unlinked. Tap “New code” to link it again.")
    elseif st.state == "pairing" and st.qr then
        return _("Waiting for you to scan the code… (it changes every 45 seconds)")
    elseif st.state == "linking" then
        return _("Scanned! Finishing the link…")
    elseif st.state == "connecting" or st.state == "connected" then
        return _("Linked! Loading your chats…")
    elseif st.state == "helper_down" then
        return _("The Signal helper is not running.")
    end
    return _("Getting a code from Signal…")
end

function QRLogin:build()
    local inner_w = self.width - 2 * Size.padding.large * 2
    local title_bar = TitleBar:new{
        width = self.width,
        fullscreen = true,
        with_bottom_line = true,
        title = _("Link Signal"),
        close_callback = function() self:onClose() end,
        show_parent = self,
    }
    local steps = TextBoxWidget:new{
        text = _("1. Open Signal on your phone.\n2. Tap your profile picture → Linked devices → Link a new device.\n3. Point the phone at this code.\n\nWhen the phone asks, choose to transfer your message history to see recent chats here."),
        face = Font:getFace("cfont", 18),
        width = inner_w,
    }
    local st = Core.status
    local qr_size = math.floor(math.min(self.width * 0.78, self.height * 0.42))
    local qr_area
    if st.state == "pairing" and st.qr then
        qr_area = QRWidget:new{
            text = st.qr,
            width = qr_size,
            height = qr_size,
            alpha = false,
        }
        self.shown_qr = st.qr
    else
        self.shown_qr = nil
        qr_area = CenterContainer:new{
            dimen = Geom:new{ w = qr_size, h = qr_size },
            FrameContainer:new{
                bordersize = Size.border.thick,
                padding = Size.padding.large,
                width = qr_size,
                height = qr_size,
                CenterContainer:new{
                    dimen = Geom:new{ w = qr_size - 2 * Size.padding.large, h = qr_size - 2 * Size.padding.large },
                    TextBoxWidget:new{
                        text = (st.state == "linking" or st.state == "connecting" or st.state == "connected")
                            and "✓" or "…",
                        face = Font:getFace("cfont", 48),
                        width = qr_size / 2,
                        alignment = "center",
                    },
                },
            },
        }
    end
    local status = TextBoxWidget:new{
        text = self:statusLine(),
        face = Font:getFace("cfont", 18),
        width = inner_w,
        alignment = "center",
        bold = true,
    }
    local can_renew = st.state ~= "linking" and st.state ~= "connecting" and st.state ~= "connected"
    local buttons = ButtonTable:new{
        width = inner_w,
        buttons = { {
            {
                text = _("New code"),
                enabled = can_renew,
                callback = function() self:restart() end,
            },
            {
                text = _("Cancel"),
                callback = function() self:onClose() end,
            },
        } },
        zero_sep = true,
        show_parent = self,
    }
    self.buttons = buttons
    local content = VerticalGroup:new{
        align = "center",
        VerticalSpan:new{ width = Size.span.vertical_large * 2 },
        steps,
        VerticalSpan:new{ width = Size.span.vertical_large * 3 },
        qr_area,
        VerticalSpan:new{ width = Size.span.vertical_large * 3 },
        status,
        VerticalSpan:new{ width = Size.span.vertical_large * 3 },
        buttons,
    }
    self[1] = FrameContainer:new{
        width = self.width,
        height = self.height,
        background = Blitbuffer.COLOR_WHITE,
        bordersize = 0,
        padding = 0,
        VerticalGroup:new{
            align = "center",
            title_bar,
            CenterContainer:new{
                dimen = Geom:new{ w = self.width, h = content:getSize().h },
                content,
            },
        },
    }
end

function QRLogin:rebuild(refresh)
    self:build()
    UIManager:setDirty(self, refresh or "ui")
end

function QRLogin:restart()
    self:requestCode()
    self:rebuild("ui")
end

function QRLogin:onSignalEvent(ev)
    if ev.type ~= "status" then return end
    local st = ev.status
    if st.linked and (st.state == "connecting" or st.state == "connected") then
        self:rebuild("ui")
        UIManager:scheduleIn(1.5, function()
            if self.closed then return end
            self.done = true
            self:onClose()
            if self.on_linked then self.on_linked() end
        end)
        return
    end
    -- a new code is a full redraw so the phone camera sees a clean image
    local qr_changed = st.qr ~= self.shown_qr
    self:rebuild(qr_changed and "full" or "ui")
end

function QRLogin:onShow()
    UIManager:setDirty(self, "full")
end

function QRLogin:onClose()
    if self.closed then return true end
    self.closed = true
    Core:removeListener(self)
    if not self.done and not Core:isLinked() and Core.status.state == "pairing" then
        Client.post("/login-cancel", {}, 3)
        Core:refreshStatus()
    end
    UIManager:close(self, "full")
    return true
end

return QRLogin
