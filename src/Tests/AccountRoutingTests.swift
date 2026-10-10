import XCTest
@testable import CLIProxyMenuBar

final class AccountRoutingTests: XCTestCase {
    func testMinimumsEnableRoutingEvenWhenOtherModeWasSelected() {
        for mode in AccountRoutingMode.allCases {
            let result = AccountRoutingConfig.compose(root: [:], mode: mode, pluginDirectory: URL(fileURLWithPath: "/plugins"), minimums: ["codex-a.json": 10])
            let plugins = ConfigComposer.stringKeyedDictionary(result["plugins"] as Any)!
            let configs = ConfigComposer.stringKeyedDictionary(plugins["configs"] as Any)!
            let own = ConfigComposer.stringKeyedDictionary(configs[AccountRoutingConfig.pluginID] as Any)!
            XCTAssertEqual(plugins["enabled"] as? Bool, true)
            XCTAssertEqual(own["enabled"] as? Bool, true)
            XCTAssertEqual(own["minimum-remaining-percent"] as? [String: Int], ["codex-a.json": 10])
        }
    }

    func testConfiguredModePreservesUserRoutingAndPlugins() {
        let root: [String: Any] = ["routing": ["strategy": "fill-first", "session-affinity": true], "plugins": ["enabled": true, "dir": "/custom/plugins"]]
        let result = AccountRoutingConfig.compose(root: root, mode: .configured, pluginDirectory: URL(fileURLWithPath: "/unused"))
        XCTAssertEqual(result as NSDictionary, root as NSDictionary)
    }

    func testQuotaModePreservesOtherPluginsAndUsesEffectiveProxy() {
        let root: [String: Any] = [
            "routing": ["strategy": "weighted-round-robin", "session-affinity": true],
            "requests": ["proxy-url": "http://proxy.example:8000"],
            "plugins": ["enabled": false, "configs": ["existing": ["enabled": true, "priority": 10, "custom": "value"]]]
        ]
        let result = AccountRoutingConfig.compose(root: root, mode: .quotaAware, pluginDirectory: URL(fileURLWithPath: "/plugins"))
        let plugins = ConfigComposer.stringKeyedDictionary(result["plugins"] as Any)!
        let configs = ConfigComposer.stringKeyedDictionary(plugins["configs"] as Any)!
        let own = ConfigComposer.stringKeyedDictionary(configs[AccountRoutingConfig.pluginID] as Any)!
        XCTAssertEqual(plugins["enabled"] as? Bool, true)
        XCTAssertEqual(plugins["dir"] as? String, "/plugins")
        XCTAssertEqual(own["priority"] as? Int, 11)
        XCTAssertEqual(own["proxy-url"] as? String, "http://proxy.example:8000")
        XCTAssertEqual(ConfigComposer.stringKeyedDictionary(configs["existing"] as Any)?["custom"] as? String, "value")
        XCTAssertEqual(ConfigComposer.stringKeyedDictionary(result["routing"] as Any)?["session-affinity"] as? Bool, true)
    }

    func testRoundRobinModeDisablesQuotaPlugin() {
        let root: [String: Any] = ["routing": ["strategy": "fill-first"]]
        let result = AccountRoutingConfig.compose(root: root, mode: .roundRobin, pluginDirectory: URL(fileURLWithPath: "/unused"))
        XCTAssertEqual(ConfigComposer.stringKeyedDictionary(result["routing"] as Any)?["strategy"] as? String, "round-robin")
        let plugins = ConfigComposer.stringKeyedDictionary(result["plugins"] as Any)!
        let configs = ConfigComposer.stringKeyedDictionary(plugins["configs"] as Any)!
        XCTAssertEqual(ConfigComposer.stringKeyedDictionary(configs[AccountRoutingConfig.pluginID] as Any)?["enabled"] as? Bool, false)
    }

    func testInstallCopiesOnlyManagedPluginAndKeepsUnrelatedFiles() throws {
        try TestSupport.withTemporaryDirectory(prefix: "routing-plugin") { directory in
            let source = directory.appendingPathComponent("source.so")
            try Data("plugin fixture".utf8).write(to: source)
            let plugins = directory.appendingPathComponent("plugins")
            try FileManager.default.createDirectory(at: plugins, withIntermediateDirectories: true)
            let unrelated = plugins.appendingPathComponent("other.so")
            try Data("other plugin".utf8).write(to: unrelated)
            try AccountRoutingConfig.installPlugin(from: source, into: plugins)
            let installed = plugins.appendingPathComponent("vibeproxy-quota.dylib")
            XCTAssertEqual(try Data(contentsOf: installed), try Data(contentsOf: source))
            XCTAssertEqual(try Data(contentsOf: unrelated), Data("other plugin".utf8))
            XCTAssertEqual(try TestSupport.filePermissions(at: installed), 0o644)
        }
    }
}
