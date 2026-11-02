// Reports whether Apple's on-device model is usable on this machine.
//
// fmux exists because `fm`'s first two backends — the on-device model and
// Private Cloud Compute — can be dead ends. This script says whether the first
// one is actually reachable here, which decides whether an on-device HTTP
// wrapper would be worth building.
//
// It uses Foundation Models, Apple's public API for third-party apps. Nothing
// here bypasses a signature or an entitlement.
//
//   xcrun swiftc -O -parse-as-library -o /tmp/probe-ondevice probe-ondevice.swift
//   /tmp/probe-ondevice
//
// Exit codes: 0 available, 2 unavailable, 3 the model was reachable but the
// request failed.
//
// Measured 2026-11-02 on macOS 27.2 / M3 Max / zh_CN: unavailable with
// reason=deviceNotEligible. The hardware is more than capable, so that verdict
// is the region gate, not the device — which is exactly the situation fmux was
// built for.

import Foundation
import FoundationModels

@main
struct ProbeOnDevice {
    static func main() async {
        let model = SystemLanguageModel.default

        switch model.availability {
        case .available:
            print("availability=available")
        case .unavailable(let reason):
            switch reason {
            case .deviceNotEligible:
                // Reported for region-gated devices too, not only old hardware.
                print("availability=unavailable reason=deviceNotEligible")
            case .appleIntelligenceNotEnabled:
                print("availability=unavailable reason=appleIntelligenceNotEnabled")
            case .modelNotReady:
                print("availability=unavailable reason=modelNotReady")
            @unknown default:
                print("availability=unavailable reason=unknown")
            }
            exit(2)
        }

        let session = LanguageModelSession()
        let started = Date()
        do {
            let response = try await session.respond(to: "Reply with exactly: OK")
            let ms = Int(Date().timeIntervalSince(started) * 1000)
            print("content=\(response.content)")
            print("latency_ms=\(ms)")
        } catch {
            print("error=\(error)")
            exit(3)
        }
    }
}
