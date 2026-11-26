import Foundation

struct CLIError: LocalizedError {
    let message: String
    var errorDescription: String? { message }
}

struct Profile: Decodable {
    let saved: Bool
    let origin: URL?
}

struct Page<Item: Decodable>: Decodable {
    let items: [Item]
    let nextCursor: String?
}

struct Machine: Decodable {
    struct Capability: Decodable {
        struct Constraints: Decodable {
            let manageAllowed: Bool?
        }

        let supported: Bool
        let status: String?
        let constraints: Constraints?
    }

    let machineId: String
    let name: String
    let powerState: String
    let revision: Int64
    let bindingGeneration: Int64
    let webUrl: URL?
    let capabilities: [String: Capability]?
    let blockedReasons: [String]?

    var operationInProgress: Bool {
        blockedReasons?.contains("operation_in_progress") == true
    }

    var status: String {
        operationInProgress ? "\(powerState.capitalized), operation in progress" : powerState.capitalized
    }

    var canUseManagedSSH: Bool {
        let capability = capabilities?["access.managed_ssh"]
        return capability?.status == "supported" && capability?.supported == true &&
            capability?.constraints?.manageAllowed == true
    }

    var website: URL? {
        guard let webUrl, webUrl.scheme == "https", let host = webUrl.host, !host.isEmpty,
              webUrl.user == nil, webUrl.password == nil, webUrl.query == nil, webUrl.fragment == nil,
              webUrl.path.isEmpty || webUrl.path == "/" else { return nil }
        return webUrl
    }

    func supports(_ key: String) -> Bool {
        guard let capability = capabilities?[key] else { return false }
        return capability.status.map { $0 == "supported" } ?? capability.supported
    }
}

struct Template: Decodable {
    struct Spec: Decodable {
        let bootMode: String?
    }

    let templateId: String
    let name: String
    let spec: Spec?
}

struct CLI {
    let executable: URL
    let options: [String]

    private static let profileEnvironment = [
        "SANDBOX_URL", "SANDBOX_TOKEN", "SANDBOX_CA_FILE", "SANDBOX_PROFILE", "SANDBOXCTL_CONFIG_DIR"
    ]

    func arguments(_ command: [String], at origin: URL?) -> [String] {
        options + (origin.map { ["--url", $0.absoluteString] } ?? []) + command
    }

    func terminalCommand(_ command: [String], at origin: URL? = nil) -> String {
        let words = ([executable.path] + arguments(command, at: origin)).map {
            "'" + $0.replacingOccurrences(of: "'", with: "'\\''") + "'"
        }
        return "unset " + Self.profileEnvironment.joined(separator: " ") + "\n" + words.joined(separator: " ")
    }

    func read<Value: Decodable>(_ command: [String], at origin: URL? = nil) throws -> Value {
        let data = try run(command, at: origin)
        let decoder = JSONDecoder()
        decoder.keyDecodingStrategy = .convertFromSnakeCase
        return try decoder.decode(Value.self, from: data)
    }

    func list<Value: Decodable>(_ command: [String], at origin: URL) throws -> [Value] {
        var items: [Value] = []
        var cursor = ""
        var seen = Set<String>()
        repeat {
            guard seen.insert(cursor).inserted else {
                throw CLIError(message: "The Platform returned a repeated page cursor.")
            }
            let page: Page<Value> = try read(command + ["--limit", "100", "--cursor", cursor], at: origin)
            items.append(contentsOf: page.items)
            cursor = page.nextCursor ?? ""
        } while !cursor.isEmpty
        return items
    }

    @discardableResult
    func run(_ command: [String], at origin: URL?, timeout: TimeInterval = 20) throws -> Data {
        let process = Process()
        process.executableURL = executable
        process.arguments = arguments(["--json"] + command, at: origin)
        var environment = ProcessInfo.processInfo.environment
        Self.profileEnvironment.forEach { environment.removeValue(forKey: $0) }
        process.environment = environment
        process.standardInput = FileHandle.nullDevice

        // Capture both streams independently so stderr cannot block a command
        // that returns a large JSON page on stdout.
        let manager = FileManager.default
        let scratch = manager.temporaryDirectory.appendingPathComponent("sandbox-menu-" + UUID().uuidString)
        try manager.createDirectory(at: scratch, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
        defer { try? manager.removeItem(at: scratch) }
        let stdout = scratch.appendingPathComponent("stdout")
        let stderr = scratch.appendingPathComponent("stderr")
        try Data().write(to: stdout)
        try Data().write(to: stderr)
        let outHandle = try FileHandle(forWritingTo: stdout)
        defer { try? outHandle.close() }
        let errHandle = try FileHandle(forWritingTo: stderr)
        defer { try? errHandle.close() }
        process.standardOutput = outHandle
        process.standardError = errHandle

        try process.run()
        let deadline = DispatchWorkItem {
            if process.isRunning { process.terminate() }
        }
        DispatchQueue.global().asyncAfter(deadline: .now() + timeout, execute: deadline)
        process.waitUntilExit()
        deadline.cancel()
        if process.terminationReason == .uncaughtSignal {
            throw CLIError(message: "The command timed out. Inspect its original operation before retrying a change.")
        }

        let result = try Data(contentsOf: stdout)
        if process.terminationStatus != 0 {
            let diagnostics = [try Data(contentsOf: stderr), result].compactMap {
                String(data: $0.prefix(8192), encoding: .utf8)?.trimmingCharacters(in: .whitespacesAndNewlines)
            }.filter { !$0.isEmpty }.joined(separator: "\n")
            throw CLIError(message: diagnostics.isEmpty ? "sandboxctl exited with status \(process.terminationStatus)." : diagnostics)
        }
        return result
    }
}
