-- mod_edgetak_rooms: written by EdgeTAK, which overwrites it when it starts.
--
-- Runs on the chat room service. Keeps the rooms made in EdgeTAK's console
-- as persistent, listed rooms: a room limited to groups is members-only,
-- with those groups' users as its members. Rooms EdgeTAK no longer lists
-- are destroyed; rooms people made themselves are left alone. Also writes
-- how many people are in each room.

local function want(name)
	local ok, m = pcall(require, "prosody." .. name);
	if ok then return m; end
	return require(name);
end

local json = want("util.json");
local lfs = require "lfs";

local state_file = assert(module:get_option_string("edgetak_state_file"), "edgetak_state_file is required");
local status_file = assert(module:get_option_string("edgetak_rooms_status_file"), "edgetak_rooms_status_file is required");
local user_host = assert(module:get_option_string("edgetak_user_host"), "edgetak_user_host is required");
local muc = module:depends("muc");
local managed = module:open_store("edgetak_rooms");

local state_mtime;

local function read_state()
	local mtime = lfs.attributes(state_file, "modification");
	if not mtime or mtime == state_mtime then return nil; end
	local f = io.open(state_file, "r");
	if not f then return nil; end
	local data = json.decode(f:read("*a") or "");
	f:close();
	if type(data) ~= "table" or type(data.rooms) ~= "table" then return nil; end
	state_mtime = mtime;
	return data.rooms;
end

local function apply(rooms)
	local wanted = {};
	for _, r in ipairs(rooms) do
		local jid = r.node .. "@" .. module.host;
		wanted[jid] = true;
		local room = muc.get_room_from_jid(jid);
		if not room then
			room = muc.create_room(jid);
			module:log("info", "Created room %s", jid);
		end
		room:set_persistent(true);
		room:set_public(true);
		room:set_name(r.name or r.node);
		room:set_description(r.description or "");
		room:set_members_only(r.members_only and true or false);
		local members = {};
		for _, u in ipairs(r.members or {}) do members[u .. "@" .. user_host] = true; end
		for member in pairs(members) do
			if room:get_affiliation(member) == nil then
				room:set_affiliation(true, member, "member");
			end
		end
		-- Members EdgeTAK no longer lists lose their place (owners and
		-- admins people chose in the room stay).
		local drop = {};
		for jid_, aff in room:each_affiliation() do
			if aff == "member" and not members[jid_] then drop[#drop + 1] = jid_; end
		end
		for _, j in ipairs(drop) do room:set_affiliation(true, j, nil); end
		room:save(true);
	end
	local before = managed:get(nil) or {};
	for jid in pairs(before) do
		if not wanted[jid] then
			local room = muc.get_room_from_jid(jid);
			if room then
				module:log("info", "Removing room %s", jid);
				room:destroy(nil, "This room was removed in EdgeTAK");
			end
		end
	end
	managed:set(nil, wanted);
end

local function write_status()
	local list = {};
	for room in muc.each_room() do
		local n = 0;
		for _ in room:each_occupant() do n = n + 1; end
		list[#list + 1] = { jid = room.jid; name = room:get_name() or ""; occupants = n; persistent = room:get_persistent() and true or false };
	end
	local tmp = status_file .. ".tmp";
	local f = io.open(tmp, "w");
	if not f then return; end
	f:write(json.encode({ rooms = list }));
	f:close();
	os.rename(tmp, status_file);
end

module:add_timer(1, function()
	local rooms = read_state();
	if rooms then
		local ok, err = pcall(apply, rooms);
		if not ok then module:log("error", "Updating rooms failed: %s", tostring(err)); end
	end
	write_status();
	return 5;
end);
