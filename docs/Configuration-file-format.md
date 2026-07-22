The config file is in [TOML] format.

Chaos Monkey will look for a file named `chaosmonkey.toml` in the following
locations:

 * `.` (current directory)
 * `/apps/chaosmonkey`
 * `/etc`
 * `/etc/chaosmonkey`

## Example

Here is an example configuration file:

[TOML]: https://github.com/toml-lang/toml

```
[chaosmonkey]
enabled = true
schedule_enabled = true
leashed = false
accounts = ["production", "test"]

[database]
host = "dbhost.example.com"
name = "chaosmonkey"
user = "chaosmonkey"
encrypted_password = "securepasswordgoeshere"

[spinnaker]
endpoint = "http://spinnaker.example.com:8084"
```

Note that while the field is called "encrypted_password", you should put the
unencrypted version of your password here. Chaos Monkey currently only ships
with a no-op (do nothing) password decryptor.


### Defaults

The following example shows all of the default values:

```
[chaosmonkey]
enabled = false                    # if false, won't terminate instances when invoked
leashed = true                     # if true, terminations are only simulated (logged only)
schedule_enabled = false           # if true, will generate schedule of terminations each weekday
accounts = []                      # list of Spinnaker accounts with chaos monkey enabled, e.g.: ["prod", "test"]

start_hour = 9                     # time during day when starts terminating
end_hour = 15                      # time during day when stops terminating

# tzdata format, see TZ column in https://en.wikipedia.org/wiki/List_of_tz_database_time_zones
# Other allowed values: "UTC", "Local"
time_zone = "America/Los_Angeles"  # time zone used by start.hour and end.hour

term_account = "root"              # account used to run the term_path command

max_apps = 2147483647              # max number of apps Chaos Monkey will schedule terminations for

# location of command Chaos Monkey uses for doing terminations
term_path = "/apps/chaosmonkey/chaosmonkey-terminate.sh"

# cron file that Chaos Monkey writes to each day for scheduling kills
cron_path = "/etc/cron.d/chaosmonkey-daily-terminations"

# decryption system for encrypted_password fields for spinnaker and database
decryptor = ""

# event tracking systems that records chaos monkey terminations
trackers = []

# metric collection systems that track errors for monitoring/alerting
error_counter = ""

# outage checking system that tells chaos monkey if there is an ongoing outage
outage_checker = ""

[database]
host = ""                # database host
port = 3306              # tcp port that the database is listening on
user = ""                # database user
encrypted_password = ""  # password for database auth, encrypted by decryptor
name = ""                # name of database that contains chaos monkey data

[spinnaker]
endpoint = ""           # spinnaker api url
certificate = ""        # path to p12 file when using client-side tls certs
encrypted_password = "" # password used for p12 certificate, encrypted by decryptor
user = ""               # user associated with terminations, sent in API call to terminate

[argocd]
enabled = false               # if true, enable the Argo CD sync/health gate + annotation write-back
endpoint = ""                 # Argo CD API server base URL, e.g. https://argocd.example.com
token = ""                    # inline bearer token (JWT); prefer token_file in production
token_file = ""               # path to a file containing the bearer token
project = ""                  # optional Argo CD project used to scope Application lookups
applications = []             # chaos-eligible Argo CD Application names/selectors
insecure_skip_verify = false  # skip TLS verification to the Argo CD server
ca_cert = ""                  # path to a PEM CA bundle to verify the Argo CD server cert
timeout = 30                  # per-request timeout in seconds

# For dynamic configuration options, see viper docs
[dynamic]
provider = ""   # options: "etcd", "consul"
endpoint = ""   # url for dynamic provider
path = ""       # path for dynamic provider
```

Note that many of these configuration parameters (decryptor, trackers,
error_counter, outage_checker) currently only have no-op implementations.

To enable the optional Argo CD integration, set `argocd.enabled = true` and
configure the `[argocd]` section above. The Argo CD sync/health gate then skips
terminating any instance whose governing Argo CD `Application` is not both
`Synced` and `Healthy`. To also record chaos actions back to Argo CD, activate
the write-back by adding `"argocd"` to the `trackers` list (for example,
`trackers = ["argocd"]`). See the [Argo CD plugin documentation](plugins/ArgoCD)
for full details.
