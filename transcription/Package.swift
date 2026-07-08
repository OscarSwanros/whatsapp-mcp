// swift-tools-version: 6.0
import PackageDescription

// On-device WhatsApp voice-note transcriber (HMB-327).
//
// A standalone SwiftPM executable. Zero third-party dependencies: it wraps
// Apple's Speech framework (SpeechAnalyzer + SpeechTranscriber, macOS 26+),
// which is inherently on-device. The only external tool is the system ffmpeg,
// invoked as a subprocess to decode WhatsApp's Ogg/Opus into a format Apple's
// audio stack can open.
let package = Package(
    name: "wa-transcribe",
    platforms: [
        .macOS("26.0")
    ],
    products: [
        .executable(name: "wa-transcribe", targets: ["WATranscribe"])
    ],
    targets: [
        .executableTarget(
            name: "WATranscribe",
            swiftSettings: [
                .swiftLanguageMode(.v6)
            ]
        )
    ]
)
