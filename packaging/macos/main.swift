import AppKit

struct MenuSettings {
    let cli: CLI
    let menuIcon: URL

    init() throws {
        var options: [String: String] = [:]
        let args = Array(CommandLine.arguments.dropFirst())
        var index = 0
        while index < args.count {
            if ["--profile", "--config-dir", "--brand-dir"].contains(args[index]), index + 1 < args.count {
                options[args[index]] = args[index + 1]
                index += 2
            } else {
                index += 1
            }
        }
        let cli = Bundle.main.bundleURL.appendingPathComponent("Contents/Helpers/sandboxctl")
        guard FileManager.default.isExecutableFile(atPath: cli.path) else {
            throw CLIError(message: "Sandbox Menu.app is missing its sandboxctl executable.")
        }
        let defaultConfig = try FileManager.default.url(for: .applicationSupportDirectory, in: .userDomainMask,
                                                        appropriateFor: nil, create: false).appendingPathComponent("sandboxctl")
        let config = options["--config-dir"].map { URL(fileURLWithPath: $0) } ?? defaultConfig
        let brand = options["--brand-dir"].map { URL(fileURLWithPath: $0) } ?? config.appendingPathComponent("brand")
        self.cli = CLI(executable: cli, options: ["--profile", options["--profile"] ?? "default", "--config-dir", config.path])
        self.menuIcon = brand.appendingPathComponent("menu-icon.png")
    }
}

final class SandboxMenu: NSObject, NSApplicationDelegate, NSMenuDelegate {
    private let settings: MenuSettings
    private let item = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
    private let menu = NSMenu()
    private let worker = DispatchQueue(label: "org.smolvm.sandbox-menu.cli", qos: .userInitiated)
    private var timer: Timer?
    private var origin: URL?
    private var machines: [Machine] = []
    private var saved = false
    private var problem: String?
    private var busy: String?
    private var lastRefresh = Date.distantPast
    private var showingDialog = false

    init(settings: MenuSettings) {
        self.settings = settings
        super.init()
    }

    func applicationDidFinishLaunching(_ notification: Notification) {
        menu.delegate = self
        menu.autoenablesItems = false
        item.menu = menu
        let icon = NSImage(contentsOf: settings.menuIcon)
        icon?.size = NSSize(width: 16, height: 16)
        icon?.isTemplate = true
        item.button?.image = icon ?? NSImage(systemSymbolName: "shippingbox", accessibilityDescription: "Sandbox Platform")
        item.button?.setAccessibilityLabel("Sandbox Platform")
        item.button?.imagePosition = .imageLeading
        let timer = Timer(timeInterval: 20, repeats: true) { [weak self] _ in
            DispatchQueue.main.async { self?.refresh() }
        }
        self.timer = timer
        RunLoop.main.add(timer, forMode: .common)
        refresh()
    }

    func menuWillOpen(_ menu: NSMenu) {
        if Date().timeIntervalSince(lastRefresh) > 4 {
            refresh()
        }
    }

    func applicationWillTerminate(_ notification: Notification) {
        timer?.invalidate()
    }

    private func rebuildMenu() {
        menu.removeAllItems()
        let running = machines.filter { $0.powerState == "running" }.count
        item.button?.title = running > 0 ? String(running) : ""
        item.button?.toolTip = running > 0 ? "\(running) running workspaces" : "Sandbox Platform"

        if let origin {
            menu.addItem(label(origin.host ?? "Sandbox Platform"))
        } else {
            menu.addItem(label("Sandbox Platform"))
        }
        if let busy {
            menu.addItem(label(busy))
        }
        if let problem {
            let error = actionItem("Show Error…", #selector(showProblem))
            error.toolTip = problem
            menu.addItem(error)
        }
        menu.addItem(.separator())

        if machines.isEmpty && busy == nil && problem == nil {
            menu.addItem(label(saved ? "No workspaces" : "Sign in to see workspaces"))
        }
        for machine in machines {
            let row = NSMenuItem(title: "\(machine.name)  ·  \(machine.status)", action: nil, keyEquivalent: "")
            row.submenu = machineMenu(machine)
            row.isEnabled = busy == nil
            menu.addItem(row)
        }

        menu.addItem(.separator())
        menu.addItem(actionItem("New Workspace…", #selector(create), enabled: saved && origin != nil && busy == nil))
        menu.addItem(actionItem(saved ? "Update Login in Terminal…" : "Sign In in Terminal…",
                                #selector(signIn), enabled: busy == nil))
        menu.addItem(actionItem("Refresh", #selector(refreshNow), enabled: busy == nil, key: "r"))
        menu.addItem(.separator())
        menu.addItem(actionItem("Quit Sandbox Menu", #selector(quit), key: "q"))
    }

    private func machineMenu(_ machine: Machine) -> NSMenu {
        let submenu = NSMenu()
        submenu.autoenablesItems = false
        if ["running", "standby", "hibernated"].contains(machine.powerState) {
            submenu.addItem(actionItem("Open Terminal", #selector(connect(_:)), value: machine.terminalArguments))
        }
        let website = actionItem("Open Website", #selector(openWebsite(_:)), value: machine.website,
                                 enabled: machine.website != nil)
        website.toolTip = machine.website?.absoluteString
        submenu.addItem(website)
        submenu.addItem(actionItem("Open in Console", #selector(openConsole(_:)), value: machine.machineId))
        if machine.operationInProgress { return submenu }

        submenu.addItem(.separator())
        switch machine.powerState {
        case "running":
            submenu.addItem(powerItem("Standby", "standby", machine, enabled: machine.supports("lifecycle.standby")))
            submenu.addItem(powerItem("Hibernate", "hibernate", machine, enabled: machine.supports("state.hibernate")))
            submenu.addItem(powerItem("Stop", "stop", machine))
        case "standby":
            submenu.addItem(powerItem("Wake", "wake-full", machine, extra: ["--from", "standby"],
                                      enabled: machine.supports("lifecycle.standby_wake")))
            submenu.addItem(powerItem("Hibernate", "hibernate", machine, enabled: machine.supports("state.hibernate")))
            submenu.addItem(powerItem("Stop", "stop", machine))
        case "hibernated":
            submenu.addItem(powerItem("Wake", "wake-full", machine, extra: ["--from", "hibernated"]))
            submenu.addItem(powerItem("Stop", "stop", machine))
        case "stopped":
            submenu.addItem(powerItem("Start", "cold-start", machine))
        default:
            submenu.addItem(label("Waiting for a reported power state"))
        }
        return submenu
    }

    private func label(_ title: String) -> NSMenuItem {
        let row = NSMenuItem(title: title, action: nil, keyEquivalent: "")
        row.isEnabled = false
        return row
    }

    private func actionItem(_ title: String, _ action: Selector, value: Any? = nil,
                            enabled: Bool = true, key: String = "") -> NSMenuItem {
        let row = NSMenuItem(title: title, action: action, keyEquivalent: key)
        row.target = self
        row.representedObject = value
        row.isEnabled = enabled
        return row
    }

    private struct PowerAction {
        let title: String
        let verb: String
        let machine: Machine
        let extra: [String]
    }

    private func powerItem(_ title: String, _ verb: String, _ machine: Machine,
                           extra: [String] = [], enabled: Bool = true) -> NSMenuItem {
        actionItem(title, #selector(changePower(_:)), value: PowerAction(title: title, verb: verb, machine: machine, extra: extra),
                   enabled: enabled)
    }

    private func perform<Value>(_ status: String, work: @escaping (CLI) throws -> Value,
                                done: @escaping (Result<Value, Error>) -> Void) {
        guard busy == nil else { return }
        busy = status
        rebuildMenu()
        let cli = settings.cli
        worker.async {
            let result = Result { try work(cli) }
            DispatchQueue.main.async {
                self.busy = nil
                done(result)
                self.rebuildMenu()
            }
        }
    }

    private func refresh() {
        guard !showingDialog else { return }
        struct Inventory {
            let profile: Profile
            let machines: [Machine]
            let error: String?
        }
        perform("Refreshing…", work: { cli in
            let profile: Profile = try cli.read(["profile", "status"])
            guard profile.saved, let origin = profile.origin else {
                return Inventory(profile: profile, machines: [], error: nil)
            }
            do {
                let machines: [Machine] = try cli.list(["machines", "list"], at: origin)
                return Inventory(profile: profile, machines: machines.sorted {
                    $0.name.localizedStandardCompare($1.name) == .orderedAscending
                }, error: nil)
            } catch {
                return Inventory(profile: profile, machines: [], error: error.localizedDescription)
            }
        }, done: { result in
            self.lastRefresh = Date()
            switch result {
            case .success(let inventory):
                self.origin = inventory.profile.origin
                self.saved = inventory.profile.saved
                self.machines = inventory.machines
                self.problem = inventory.error
            case .failure(let error):
                self.origin = nil
                self.saved = false
                self.machines = []
                self.problem = error.localizedDescription
            }
        })
    }

    @objc private func refreshNow() { refresh() }
    @objc private func quit() { NSApp.terminate(self) }

    @objc private func showProblem() {
        if let problem { showAlert("Sandbox Platform", problem) }
    }

    @objc private func signIn() { openTerminal(["login", "--no-ssh-config"]) }

    @objc private func connect(_ sender: NSMenuItem) {
        guard let arguments = sender.representedObject as? [String], let origin else { return }
        openTerminal(arguments, at: origin)
    }

    @objc private func openConsole(_ sender: NSMenuItem) {
        guard let id = sender.representedObject as? String, let origin,
              var url = URLComponents(url: origin, resolvingAgainstBaseURL: false) else { return }
        url.path = "/"
        url.fragment = "/machines/" + id
        if let target = url.url, !NSWorkspace.shared.open(target) {
            showAlert("Couldn't Open Console", target.absoluteString)
        }
    }

    @objc private func openWebsite(_ sender: NSMenuItem) {
        guard let target = sender.representedObject as? URL else { return }
        if !NSWorkspace.shared.open(target) {
            showAlert("Couldn't Open Website", target.absoluteString)
        }
    }

    @objc private func create() {
        guard let origin else { return }
        perform("Loading templates…", work: { cli in
            try cli.list(["templates", "list"], at: origin) as [Template]
        }, done: { result in
            switch result {
            case .success(let templates):
                self.showCreateDialog(templates, at: origin)
            case .failure(let error):
                self.showAlert("Couldn't Load Templates", error.localizedDescription)
            }
        })
    }

    private func showCreateDialog(_ templates: [Template], at origin: URL) {
        guard !templates.isEmpty else {
            showAlert("No Registered Templates", "Register a template before creating a workspace.")
            return
        }
        let name = NSTextField(string: "")
        name.placeholderString = "Workspace name"
        name.setAccessibilityLabel("Workspace name")
        let choice = NSPopUpButton(frame: .zero, pullsDown: false)
        choice.setAccessibilityLabel("Template")
        choice.addItems(withTitles: templates.map { "\($0.name) (\($0.templateId))" })
        let form = NSStackView(views: [name, choice])
        form.orientation = .vertical
        form.alignment = .leading
        form.spacing = 8
        form.translatesAutoresizingMaskIntoConstraints = false
        let accessory = NSView(frame: NSRect(x: 0, y: 0, width: 320, height: 60))
        accessory.addSubview(form)
        NSLayoutConstraint.activate([
            form.leadingAnchor.constraint(equalTo: accessory.leadingAnchor),
            form.trailingAnchor.constraint(equalTo: accessory.trailingAnchor),
            form.topAnchor.constraint(equalTo: accessory.topAnchor),
            name.widthAnchor.constraint(equalTo: form.widthAnchor),
            choice.widthAnchor.constraint(equalTo: form.widthAnchor)
        ])

        let alert = NSAlert()
        alert.messageText = "New Workspace"
        alert.accessoryView = accessory
        alert.addButton(withTitle: "Create")
        alert.addButton(withTitle: "Cancel")
        alert.window.initialFirstResponder = name
        while present(alert) == .alertFirstButtonReturn {
            let value = name.stringValue.trimmingCharacters(in: .whitespacesAndNewlines)
            guard !value.isEmpty, templates.indices.contains(choice.indexOfSelectedItem) else {
                alert.informativeText = "Enter a name and select a template."
                continue
            }
            let template = templates[choice.indexOfSelectedItem]
            var command = ["machines", "create", "--template", template.templateId]
            if let mode = template.spec?.bootMode, ["system", "oci"].contains(mode) {
                command += ["--boot-mode", mode]
            }
            mutate(command + ["--", value], title: "Creating workspace…", at: origin)
            return
        }
    }

    @objc private func changePower(_ sender: NSMenuItem) {
        guard let power = sender.representedObject as? PowerAction, let origin else { return }
        switch power.verb {
        case "standby", "hibernate", "stop":
            let text: String
            switch power.verb {
            case "standby": text = "The running workspace will pause and keep its live state on the host."
            case "hibernate": text = "The workspace will save its execution state and release the running VM."
            default: text = "The workspace will shut down and in-memory work may be lost."
            }
            let alert = NSAlert()
            alert.messageText = "\(power.title) \(power.machine.name)?"
            alert.informativeText = text
            alert.addButton(withTitle: "Cancel")
            alert.addButton(withTitle: power.title).hasDestructiveAction = power.verb == "stop"
            guard present(alert) == .alertSecondButtonReturn else { return }
        default:
            break
        }
        var command = ["machines", power.verb] + power.extra
        if power.machine.bindingGeneration > 0 && power.machine.revision > 0 {
            command += ["--generation", String(power.machine.bindingGeneration), "--revision", String(power.machine.revision)]
        }
        mutate(command + ["--", power.machine.machineId], title: "\(power.title) · \(power.machine.name)", at: origin)
    }

    private func mutate(_ command: [String], title: String, at origin: URL) {
        perform(title, work: { cli in
            try cli.run(["--strict-result"] + command, at: origin, timeout: 200)
        }, done: { result in
            if case .failure(let error) = result {
                self.showAlert(title, error.localizedDescription)
            }
            self.refresh()
        })
    }

    private func openTerminal(_ arguments: [String], at origin: URL? = nil) {
        let manager = FileManager.default
        let directory = manager.temporaryDirectory.appendingPathComponent("sandbox-terminal-" + UUID().uuidString)
        let script = directory.appendingPathComponent("Sandbox.command")
        do {
            try manager.createDirectory(at: directory, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
            let command = settings.cli.terminalCommand(arguments, at: origin)
            let text = "#!/bin/sh\n/usr/bin/unlink \"$0\"\n/bin/rmdir \"${0%/*}\"\n" + command + "\n"
            try text.write(to: script, atomically: true, encoding: .utf8)
            try manager.setAttributes([.posixPermissions: 0o700], ofItemAtPath: script.path)
            guard let terminal = NSWorkspace.shared.urlForApplication(withBundleIdentifier: "com.apple.Terminal") else {
                throw CLIError(message: "Terminal.app could not be found.")
            }
            NSWorkspace.shared.open([script], withApplicationAt: terminal, configuration: .init()) { _, error in
                guard let error else { return }
                try? manager.removeItem(at: directory)
                DispatchQueue.main.async { self.showAlert("Couldn't Open Terminal", error.localizedDescription) }
            }
        } catch {
            try? manager.removeItem(at: directory)
            showAlert("Couldn't Open Terminal", error.localizedDescription)
        }
    }

    private func showAlert(_ title: String, _ detail: String) {
        let alert = NSAlert()
        alert.messageText = title
        alert.informativeText = detail
        _ = present(alert)
    }

    private func present(_ alert: NSAlert) -> NSApplication.ModalResponse {
        showingDialog = true
        defer { showingDialog = false }
        NSApp.activate(ignoringOtherApps: true)
        return alert.runModal()
    }
}

let app = NSApplication.shared
app.setActivationPolicy(.accessory)
do {
    let delegate = SandboxMenu(settings: try MenuSettings())
    app.delegate = delegate
    withExtendedLifetime(delegate) { app.run() }
} catch {
    let alert = NSAlert(error: error)
    app.activate(ignoringOtherApps: true)
    alert.runModal()
}
