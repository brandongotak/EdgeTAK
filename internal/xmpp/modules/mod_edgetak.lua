-- mod_edgetak: written by EdgeTAK, which overwrites it when it starts.
--
-- Keeps Prosody in step with EdgeTAK through two files in EdgeTAK's data
-- directory: it reads the state EdgeTAK writes (users and their groups) and
-- writes back who is online. Users see the other members of their EdgeTAK
-- groups as contacts (as Prosody's mod_groups does, from EdgeTAK's list),
-- and a user who is removed or disabled in EdgeTAK is disconnected.

local function want(name)
	local ok, m = pcall(require, "prosody." .. name);
	if ok then return m; end
	return require(name);
end

local json = want("util.json");
local datamanager = want("util.datamanager");
local lfs = require "lfs";

local state_file = assert(module:get_option_string("edgetak_state_file"), "edgetak_state_file is required");
local status_file = assert(module:get_option_string("edgetak_status_file"), "edgetak_status_file is required");
local host = module.host;

local state = { users = {}, groups = {} };
local state_mtime;

local function load_state()
	local mtime = lfs.attributes(state_file, "modification");
	if not mtime or mtime == state_mtime then return false; end
	local f = io.open(state_file, "r");
	if not f then return false; end
	local data = json.decode(f:read("*a") or "");
	f:close();
	if type(data) ~= "table" or type(data.users) ~= "table" then
		module:log("warn", "EdgeTAK's state file is not valid JSON");
		return false;
	end
	state, state_mtime = data, mtime;
	return true;
end

-- Contacts: everyone sharing a group, with their EdgeTAK name.
module:hook("roster-load", function(event)
	if event.host ~= host then return; end
	load_state();
	local me = event.username;
	local roster = event.roster;
	for group, list in pairs(state.groups or {}) do
		local member = false;
		for _, u in ipairs(list) do if u == me then member = true; break; end end
		if member then
			for _, u in ipairs(list) do
				if u ~= me then
					local jid = u .. "@" .. host;
					local item = roster[jid] or {};
					roster[jid] = item;
					item.subscription = "both";
					item.name = state.users[u] or u;
					item.groups = item.groups or {};
					item.groups[group] = true;
					item.persist = false;
				end
			end
		end
	end
	if roster[false] then roster[false].version = true; end
end);

-- Those contacts are never saved into the user's own roster.
local function remove_virtual_contacts(username, store_host, datastore, data)
	if store_host == host and datastore == "roster" and type(data) == "table" then
		local kept = {};
		for jid, contact in pairs(data) do
			if type(contact) ~= "table" or contact.persist ~= false then kept[jid] = contact; end
		end
		if kept[false] then kept[false].version = nil; end
		return username, store_host, datastore, kept;
	end
	return username, store_host, datastore, data;
end
datamanager.add_callback(remove_virtual_contacts);
function module.unload() datamanager.remove_callback(remove_virtual_contacts); end

local function write_status()
	local sessions = {};
	local host_sessions = prosody.hosts[host] and prosody.hosts[host].sessions or {};
	for username, user in pairs(host_sessions) do
		for resource, session in pairs(user.sessions or {}) do
			local show = "online";
			if session.presence then
				local s = session.presence:get_child_text("show");
				if s then show = s; end
			elseif not session.presence then
				show = "connecting";
			end
			sessions[#sessions + 1] = {
				user = username; resource = resource; ip = session.ip or "";
				since = session.conntime or 0; show = show;
				secure = session.secure and true or false;
			};
		end
	end
	local tmp = status_file .. ".tmp";
	local f = io.open(tmp, "w");
	if not f then return; end
	f:write(json.encode({ host = host; version = prosody.version; started = prosody.start_time; at = os.time(); sessions = sessions }));
	f:close();
	os.rename(tmp, status_file);
end

-- Disconnect users EdgeTAK no longer lists (removed or disabled).
local function kick_removed()
	local host_sessions = prosody.hosts[host] and prosody.hosts[host].sessions or {};
	for username, user in pairs(host_sessions) do
		if state.users[username] == nil then
			for _, session in pairs(user.sessions or {}) do
				module:log("info", "Disconnecting %s: no longer an EdgeTAK user", username);
				session:close({ condition = "not-authorized"; text = "Your EdgeTAK account was removed or disabled" });
			end
		end
	end
end

module:add_timer(1, function()
	if load_state() then kick_removed(); end
	write_status();
	return 5;
end);

module:hook("resource-bind", function() module:add_timer(0.5, write_status); end);
module:hook("resource-unbind", function() module:add_timer(0.5, write_status); end);
