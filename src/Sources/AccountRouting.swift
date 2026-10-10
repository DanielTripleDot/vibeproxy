import Foundation

enum AccountRoutingMode: String, CaseIterable, Identifiable {
    case configured
    case roundRobin = "round-robin"
    case quotaAware = "quota-aware"

    var id: String { rawValue }
    var title: String {
        switch self {
        case .configured: return "Use configuration"
        case .roundRobin: return "Round robin"
        case .quotaAware: return "Quota-aware"
        }
    }
}

enum AccountRoutingConfig {
    static let pluginID = "vibeproxy-quota"

    static func pluginDirectory(in root: [String: Any], authDirectory: URL) -> URL {
        let plugins = ConfigComposer.stringKeyedDictionary(root["plugins"] as Any) ?? [:]
        if let path = ConfigComposer.normalizedString(plugins["dir"]) {
            return URL(fileURLWithPath: (path as NSString).expandingTildeInPath, isDirectory: true)
        }
        return authDirectory.appendingPathComponent("plugins", isDirectory: true)
    }

    static func compose(root: [String: Any], mode: AccountRoutingMode, pluginDirectory: URL, minimums: [String: Int] = [:]) -> [String: Any] {
        let minimums = minimums.filter { !$0.key.isEmpty && (1...100).contains($0.value) }
        // A saved reserve must never silently stop being enforced after a mode change.
        let mode: AccountRoutingMode = minimums.isEmpty ? mode : .quotaAware
        guard mode != .configured else { return root }
        var result = root
        var plugins = ConfigComposer.stringKeyedDictionary(root["plugins"] as Any) ?? [:]
        var configs = ConfigComposer.stringKeyedDictionary(plugins["configs"] as Any) ?? [:]
        var ownConfig = ConfigComposer.stringKeyedDictionary(configs[pluginID] as Any) ?? [:]
        ownConfig["enabled"] = mode == .quotaAware
        ownConfig["minimum-remaining-percent"] = minimums
        if mode == .quotaAware {
            plugins["enabled"] = true
            plugins["dir"] = pluginDirectory.path
            // This opt-in mode owns account selection. Existing plugins remain
            // installed; return to Use configuration to restore their routing.
            let highestPriority = configs.values.compactMap {
                ConfigComposer.stringKeyedDictionary($0)?["priority"] as? Int
            }.max() ?? 0
            ownConfig["priority"] = highestPriority < Int.max ? highestPriority + 1 : highestPriority
            let requests = ConfigComposer.stringKeyedDictionary(root["requests"] as Any) ?? [:]
            ownConfig["proxy-url"] = (requests["proxy-url"] as? String) ?? (root["proxy-url"] as? String) ?? ""
        } else {
            var routing = ConfigComposer.stringKeyedDictionary(root["routing"] as Any) ?? [:]
            routing["strategy"] = "round-robin"
            result["routing"] = routing
        }
        configs[pluginID] = ownConfig
        plugins["configs"] = configs
        result["plugins"] = plugins
        return result
    }

    static func installPlugin(from source: URL, into directory: URL) throws {
        let data = try Data(contentsOf: source)
        let destination = directory.appendingPathComponent("\(pluginID).dylib")
        if (try? Data(contentsOf: destination)) == data { return }
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        try data.write(to: destination, options: .atomic)
        try FileManager.default.setAttributes([.posixPermissions: 0o644], ofItemAtPath: destination.path)
    }
}
