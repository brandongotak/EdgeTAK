-- mod_auth_edgetak: written by EdgeTAK, which overwrites it when it starts.
--
-- Users sign in with their EdgeTAK username and password (TAK Chat's "Use
-- TAK Server Credentials" reuses the ones ATAK keeps, which after a
-- quick-connect QR code is the code), or with their device's EdgeTAK
-- certificate (SASL EXTERNAL, for apps that do it): Prosody asks EdgeTAK (on a local address, with a
-- shared secret), so disabled users, revoked certificates, LDAP accounts and
-- EdgeTAK's login protection all apply. Accounts are made and changed in
-- EdgeTAK, never here.

local function want(name)
	local ok, m = pcall(require, "prosody." .. name);
	if ok then return m; end
	return require(name);
end

local new_sasl = want("util.sasl").new;
local http = want("net.http");
local async = want("util.async");
local json = want("util.json");

local auth_url = assert(module:get_option_string("edgetak_auth_url"), "edgetak_auth_url is required");
local secret = assert(module:get_option_string("edgetak_secret"), "edgetak_secret is required");
local state_file = module:get_option_string("edgetak_state_file");

-- user_exists answers from the user list EdgeTAK writes (it is called where
-- Prosody can't wait for a request), re-read when it changes.
local lfs = require "lfs";
local users, users_mtime = {}, nil;
local function current_users()
	if not state_file then return users; end
	local mtime = lfs.attributes(state_file, "modification");
	if mtime and mtime ~= users_mtime then
		local f = io.open(state_file, "r");
		if f then
			local data = json.decode(f:read("*a") or "");
			f:close();
			if type(data) == "table" and type(data.users) == "table" then
				users, users_mtime = data.users, mtime;
			end
		end
	end
	return users;
end

local function call(body)
	if not async.ready() then
		module:log("warn", "sign-in check outside an async context");
		return nil;
	end
	local res, err = async.wait_for(http.request(auth_url, {
		method = "POST";
		body = json.encode(body);
		headers = { ["Content-Type"] = "application/json"; ["Authorization"] = "Bearer " .. secret };
	}));
	if not res then
		module:log("error", "EdgeTAK did not answer a sign-in check: %s", tostring(err));
	end
	return res;
end

local function ask(username, password, ip)
	local res = call({ username = username; password = password; ip = ip or "" });
	return res and res.code == 200;
end

-- The certificate the client presented, as PEM, if any.
local function peer_cert(session)
	local conn = session and session.conn;
	if not (conn and conn.ssl_peercertificate) then return nil; end
	local ok, cert = pcall(conn.ssl_peercertificate, conn);
	if ok and cert and cert.pem then return cert:pem(); end
	return nil;
end

-- Which user a certificate signs in as, according to EdgeTAK.
local function ask_cert(pem, ip)
	local res = call({ cert = pem; ip = ip or "" });
	if not (res and res.code == 200) then return nil; end
	local data = json.decode(res.body or "");
	return type(data) == "table" and data.username or nil;
end

local provider = {};

function provider.test_password(username, password)
	return ask(username, password, nil);
end

function provider.user_exists(username)
	return current_users()[username] ~= nil;
end

function provider.users()
	local list = {};
	for name in pairs(current_users()) do list[#list + 1] = name; end
	local i = 0;
	return function() i = i + 1; return list[i]; end;
end

function provider.set_password()
	return nil, "Change your password in EdgeTAK.";
end

function provider.create_user()
	return nil, "Accounts are made in EdgeTAK.";
end

function provider.delete_user()
	return nil, "Accounts are managed in EdgeTAK.";
end

function provider.get_sasl_handler(session)
	local ip = session and session.ip;
	local profile = {
		plain_test = function(_, username, password)
			return ask(username, password, ip), true;
		end;
	};
	-- EXTERNAL is offered only to a client that presented a certificate.
	local pem = peer_cert(session);
	if pem then
		profile.external = function(authzid)
			local user = ask_cert(pem, ip);
			if not user then return nil, nil; end
			if authzid and authzid ~= "" and authzid ~= user and authzid ~= user .. "@" .. module.host then
				module:log("info", "Certificate for %s used to sign in as %s: refused", user, authzid);
				return nil, nil;
			end
			return user, true;
		end;
	end
	return new_sasl(module.host, profile);
end

module:provides("auth", provider);
