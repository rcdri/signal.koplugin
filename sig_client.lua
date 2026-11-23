--[[--
Talks to sig-helper over HTTP (localhost by default).

Calls are short and blocking; the helper answers from its own database,
so they return in milliseconds.

@module koplugin.signal.client
]]

local http = require("socket.http")
local ltn12 = require("ltn12")
local logger = require("logger")
local rapidjson = require("rapidjson")
local socket = require("socket")
local socketutil = require("socketutil")

local Client = {
    base = "http://127.0.0.1:18782",
    token = "",
    emoji_names = true,
}

local function urlencode(s)
    return (tostring(s):gsub("[^%w%-_%.~]", function(c) return string.format("%%%02X", c:byte()) end))
end

function Client.path(path, query)
    if not query then return path end
    local parts = {}
    for k, v in pairs(query) do
        if v ~= nil and v ~= "" then
            table.insert(parts, urlencode(k) .. "=" .. urlencode(v))
        end
    end
    table.sort(parts)
    if #parts == 0 then return path end
    return path .. "?" .. table.concat(parts, "&")
end

local function headers(body_len)
    local h = {
        ["Authorization"] = "Bearer " .. Client.token,
        ["Accept"] = "application/json",
    }
    if Client.emoji_names then h["X-Emoji"] = "names" end
    if body_len then
        h["Content-Type"] = "application/json"
        h["Content-Length"] = tostring(body_len)
    end
    return h
end

--- Performs a request. Returns the decoded JSON body, or nil, error message, HTTP code.
function Client.call(method, path, body, timeout)
    local src = body and rapidjson.encode(body) or nil
    if method == "POST" and not src then src = "{}" end
    local sink = {}
    timeout = timeout or 5
    socketutil:set_timeout(timeout, timeout + 2)
    local ok, code, resp_headers = pcall(function()
        return socket.skip(1, http.request{
            url = Client.base .. path,
            method = method,
            headers = headers(src and #src),
            source = src and ltn12.source.string(src) or nil,
            sink = ltn12.sink.table(sink),
        })
    end)
    socketutil:reset_timeout()
    if not ok then
        return nil, tostring(code), 0
    end
    if type(code) ~= "number" then
        return nil, tostring(code or resp_headers or "no response"), 0
    end
    local text = table.concat(sink)
    local data = text ~= "" and rapidjson.decode(text) or {}
    if code >= 400 then
        local err = type(data) == "table" and data.error or ("HTTP " .. code)
        logger.dbg("signal:", method, path, "->", code, err)
        return nil, err, code, data
    end
    if type(data) ~= "table" then
        return nil, "bad response", code
    end
    return data, nil, code
end

function Client.get(path, query, timeout)
    return Client.call("GET", Client.path(path, query), nil, timeout)
end

function Client.post(path, body, timeout)
    return Client.call("POST", path, body or {}, timeout)
end

--- Downloads a binary response to a file. Returns true, or nil and an error.
function Client.download(path, query, dest, timeout)
    local tmp = dest .. ".part"
    local f, ferr = io.open(tmp, "wb")
    if not f then return nil, ferr end
    timeout = timeout or 120
    socketutil:set_timeout(timeout, timeout)
    local errbuf = {}
    local status
    local ok, code = pcall(function()
        return socket.skip(1, http.request{
            url = Client.base .. Client.path(path, query),
            method = "GET",
            headers = headers(),
            sink = function(chunk, err)
                if chunk == nil then return 1 end
                if status and status ~= 200 then
                    table.insert(errbuf, chunk)
                    return 1
                end
                return f:write(chunk) and 1 or nil
            end,
        })
    end)
    status = ok and code or nil
    socketutil:reset_timeout()
    f:close()
    if not ok or type(code) ~= "number" then
        os.remove(tmp)
        return nil, tostring(code)
    end
    if code ~= 200 then
        -- the error body was written to the file before we knew the status
        local fe = io.open(tmp, "rb")
        local text = fe and fe:read("*a") or ""
        if fe then fe:close() end
        os.remove(tmp)
        local data = text ~= "" and rapidjson.decode(text)
        return nil, type(data) == "table" and data.error or ("HTTP " .. code)
    end
    os.remove(dest)
    local renamed, rerr = os.rename(tmp, dest)
    if not renamed then return nil, rerr end
    return true
end

return Client
