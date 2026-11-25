# Debian System guest services

Use a prepared Debian Linux x86-64 root filesystem with `systemd-sysv` installed. Place the required binaries in a single directory and run the installer against the template root before assembling its ext4 root disk.

```sh
guest/templates/system-debian/install.sh "$rootfs" "$binaries" "$managed_uplink"
```

The installer enables `smolvm-agent.service` and `sandbox-taskd.service` in the guest. Supply a musl `smolvm-agent` built with the System service entry point and Linux x86-64 builds of `taskd`, `taskctl` and `task-runner`.

Set `managed_uplink` to the interface assigned to the guest identity service. The installer marks just that interface unmanaged by systemd-networkd. The identity service configures the uplink address and routes.

Use libkrun's block root and PID 1 settings to boot the Debian init from the root disk. The Agent serves health and foreground command requests over the private vsock. The task daemon serves its existing guest-local socket.

On templates with Debian OpenSSH installed, the installer also configures `sandbox-managed-sshd.service` on guest TCP port 2222. This listener uses its own key and authorization files under `/etc/ssh/sandbox-managed`. Set `managed_ssh` to `true` in the registered System template specification to provision keys through the execution-scoped Agent. The guest helper verifies the key presented by the actual listener. A rotation restarts the managed listener only when it was active. Initial provisioning starts the listener when its unit is enabled.
