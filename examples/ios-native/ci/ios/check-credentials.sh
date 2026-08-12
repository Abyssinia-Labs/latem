#!/usr/bin/env bash
set -euo pipefail

# Every ci/ script sources this: it puts Homebrew Ruby on PATH and sets the
# locale, neither of which a runner step inherits. See ci/lib/env.sh.
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/../lib/env.sh"

# ---------------------------------------------------------------------------
# Isolate which App Store Connect credential is wrong, in about a second.
#
# `fastlane pilot upload` reports all three failures identically:
#
#   Authentication credentials are missing or invalid. - Provide a properly
#   configured and signed bearer token...
#
# — after it has archived, exported, and uploaded. That is a ~2 minute round
# trip per guess against three independent values, one of which (the PEM) is not
# something you can eyeball. This does the same authentication against a
# read-only endpoint and says which one Apple rejected.
#
# Prints NO secret material: key ids are shown, the PEM never is.
#
# Requires the same environment as ci/ios/build.sh:
#   APP_STORE_CONNECT_KEY_ID, APP_STORE_CONNECT_ISSUER_ID, APP_STORE_CONNECT_KEY_P8
# ---------------------------------------------------------------------------

: "${APP_STORE_CONNECT_KEY_ID:?not set}"
: "${APP_STORE_CONNECT_ISSUER_ID:?not set}"
: "${APP_STORE_CONNECT_KEY_P8:?not set}"

ruby -rjson -ropenssl -rbase64 -rnet/http -rtime <<'RUBY'
key_id    = ENV.fetch("APP_STORE_CONNECT_KEY_ID")
issuer_id = ENV.fetch("APP_STORE_CONNECT_ISSUER_ID")
pem       = ENV.fetch("APP_STORE_CONNECT_KEY_P8")

fail_count = 0
def ok(m)   = puts("  \e[32m✓\e[0m #{m}")
def bad(m)  = puts("  \e[31m✗\e[0m #{m}")
def note(m) = puts("    #{m}")

puts "\n== shape =="

# Key ID: 10 alphanumerics. Apple names the download AuthKey_<KEY_ID>.p8, so a
# mismatch here usually means two keys were created and the ids got crossed.
if key_id.match?(/\A[A-Z0-9]{10}\z/)
  ok "key id      #{key_id}"
else
  bad "key id      #{key_id.inspect} — expected 10 uppercase alphanumerics"
  note "it is the XXXXXXXXXX in the AuthKey_XXXXXXXXXX.p8 you downloaded"
  fail_count += 1
end

# Issuer ID is a UUID. The single most common mistake is pasting the key id
# here, or an App ID, or the team id — none of which are UUIDs.
if issuer_id.match?(/\A[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\z/i)
  ok "issuer id   #{issuer_id}"
else
  bad "issuer id   #{issuer_id.inspect} — expected a UUID"
  note "App Store Connect → Users and Access → Integrations → App Store Connect API."
  note "It is above the key table, labelled 'Issuer ID' — one per team, not per key."
  fail_count += 1
end

key = nil
begin
  key = OpenSSL::PKey::EC.new(pem)
  raise "not P-256" unless key.group.curve_name == "prime256v1"
  ok "private key parses (EC P-256, #{pem.lines.count} lines)"
rescue => e
  bad "private key does not parse: #{e.message}"
  note "must be the CONTENTS of the .p8, starting -----BEGIN PRIVATE KEY-----"
  fail_count += 1
end

# JWTs carry iat/exp. A host clock more than a few minutes off produces a token
# Apple rejects as expired or not-yet-valid, reported as the same generic error.
skew = nil
begin
  h = Net::HTTP.new("api.appstoreconnect.apple.com", 443).tap { |x| x.use_ssl = true }
  r = h.head("/v1/apps")
  skew = (Time.now - Time.httpdate(r["date"])).abs if r["date"]
rescue
end
if skew.nil?
  note "clock skew   could not be measured (no network?)"
elsif skew < 60
  ok "clock skew  #{skew.round}s"
else
  bad "clock skew  #{skew.round}s — a JWT signed by this host may be rejected"
  fail_count += 1
end

# Which .p8 did this key material actually come from?
#
# Apple names every download AuthKey_<KEY_ID>.p8, so the filename IS the key id.
# If the PEM in the environment fingerprints to a file whose name disagrees with
# APP_STORE_CONNECT_KEY_ID, the JWT carries a `kid` that does not match its own
# signature, and Apple returns a 401 that mentions neither. Anyone who has made
# more than one key over the years — an APNs key, an older CI key — has several
# of these files, all 257 bytes, all looking alike.
#
# Public-key fingerprints only. No private material is read out or printed.
if key
  fp = ->(k) { OpenSSL::Digest::SHA256.hexdigest(k.public_key.to_octet_string(:uncompressed))[0, 16] }
  mine = fp.call(key)
  found = []
  %w[~/Downloads ~/Desktop ~/Documents ~/.appstoreconnect/private_keys ~/.private_keys].each do |dir|
    Dir.glob(File.expand_path("#{dir}/**/AuthKey_*.p8")).each do |f|
      begin
        found << [File.basename(f)[/AuthKey_(\w+)\.p8/, 1], fp.call(OpenSSL::PKey::EC.new(File.read(f))), f]
      rescue
      end
    end
  end
  match = found.find { |_id, f, _path| f == mine }
  if match
    if match[0] == key_id
      ok "key material matches AuthKey_#{match[0]}.p8, and the key id agrees"
    else
      bad "KEY ID MISMATCH"
      note "APP_STORE_CONNECT_KEY_ID is #{key_id}"
      note "but the key material is from AuthKey_#{match[0]}.p8"
      note ""
      note "The JWT would claim kid=#{key_id} while being signed by #{match[0]}."
      note "Apple rejects that as 401 without saying which half is wrong."
      note ""
      note "fix: export APP_STORE_CONNECT_KEY_ID=#{match[0]}"
      note "     (and confirm that is the key you created, in App Store Connect)"
      fail_count += 1
    end
  elsif found.any?
    note "key material matches none of the #{found.count} AuthKey_*.p8 on disk"
    note "(that is fine if you pasted it from elsewhere)"
  end
end

if fail_count > 0 || key.nil?
  puts "\n#{fail_count} problem(s) above; not contacting Apple.\n\n"
  exit 1
end

puts "\n== authenticating against App Store Connect =="

# ES256 JWT, by hand. OpenSSL signs to DER; JWS wants raw r||s, 32 bytes each.
def b64(s) = Base64.urlsafe_encode64(s).delete("=")
def der_to_raw(der)
  a = OpenSSL::ASN1.decode(der)
  a.value.map { |v| v.value.to_s(2).rjust(32, "\x00") }.join
end

now = Time.now.to_i
header = { alg: "ES256", kid: key_id, typ: "JWT" }

# Apple issues two kinds of key and they are NOT interchangeable in the token.
#
#   Team Key       payload carries iss: <issuer id>
#   Individual Key payload omits iss entirely and carries sub: "user"
#
# Both are created on the same App Store Connect page, both hand you an
# AuthKey_XXXXXXXXXX.p8, both show an Issuer ID on screen — and using the wrong
# payload shape returns exactly the same 401 as a wrong key would. So try both
# and let Apple say which one this is. fastlane's spaceship only builds the team
# form, which is why an individual key fails there no matter how correct it is.
forms = {
  "team key (iss)"          => { iss: issuer_id, iat: now, exp: now + 900, aud: "appstoreconnect-v1" },
  "individual key (sub)"    => { sub: "user",    iat: now, exp: now + 900, aud: "appstoreconnect-v1" },
}

http = Net::HTTP.new("api.appstoreconnect.apple.com", 443).tap { |h| h.use_ssl = true }
res = nil
accepted_form = nil

forms.each do |label, payload|
  signing_input = "#{b64(JSON.generate(header))}.#{b64(JSON.generate(payload))}"
  jwt = "#{signing_input}.#{b64(der_to_raw(key.sign(OpenSSL::Digest.new('SHA256'), signing_input)))}"
  r = http.get("/v1/apps?limit=200", { "Authorization" => "Bearer #{jwt}" })
  puts "  #{r.code == '200' ? "\e[32m✓\e[0m" : "\e[31m✗\e[0m"} #{label.ljust(22)} HTTP #{r.code}"
  if r.code == "200" && res.nil?
    res = r
    accepted_form = label
  end
end
res ||= http.get("/v1/apps?limit=200", { "Authorization" => "Bearer invalid" })

if accepted_form == "individual key (sub)"
  puts ""
  bad "this is an INDIVIDUAL key, and fastlane cannot use it"
  note "spaceship builds only the team-key token, so pilot will keep returning 401."
  note "fix: App Store Connect → Users and Access → Integrations →"
  note "     App Store Connect API → *Team Keys* tab → + , role App Manager."
  note "     The individual key can stay; it is simply not usable here."
  exit 1
end

case res.code
when "200"
  apps = JSON.parse(res.body)["data"] || []
  ok "authenticated — the API accepted this key"
  puts "\n  apps visible to this key (#{apps.count}):"
  apps.each { |a| puts "    #{a.dig('attributes','bundleId')}  — #{a.dig('attributes','name')}" }
  want = "com.abyssinialabs.latemexample"
  if apps.any? { |a| a.dig("attributes", "bundleId") == want }
    ok "#{want} is present"
  else
    puts ""
    bad "#{want} is NOT visible to this key"
    note "Either the app record does not exist (App Store Connect → Apps → +),"
    note "or this key's role cannot see it. Uploading will fail even though"
    note "authentication just succeeded."
    exit 1
  end
when "401"
  bad "401 Unauthorized — the values are well-formed but Apple rejected them"
  note ""
  note "In likelihood order:"
  note "  1. issuer_id belongs to a different team, or is simply the wrong UUID."
  note "  2. key_id and the .p8 are from different keys (two keys were made)."
  note "  3. the key was revoked in App Store Connect."
  note "  4. it is an INDIVIDUAL key, not a TEAM key. Individual keys are issued"
  note "     under Users and Access → Integrations → individual, and do not use"
  note "     the team Issuer ID. Re-create it under 'Team Keys'."
  exit 1
when "403"
  bad "403 Forbidden — authentication worked, authorisation did not"
  note "The key's role is too weak. TestFlight uploads need App Manager;"
  note "Developer is not sufficient. Change it in App Store Connect."
  exit 1
else
  bad "HTTP #{res.code}"
  note res.body.to_s[0, 400]
  exit 1
end
RUBY
