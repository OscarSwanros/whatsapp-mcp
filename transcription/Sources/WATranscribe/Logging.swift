import Foundation
import OSLog

/// Diagnostic logging. Anything derived from voice-note content is `.private`
/// by default — voice notes are personal PII and must never land in a log at
/// `.public`. Structured diagnostics go through `os.Logger`; the transcript
/// itself is the CLI's product and is the only thing written to stdout.
enum WALog {
    static let subsystem = "com.oscarswanros.wa-transcribe"
    static let transcription = Logger(subsystem: subsystem, category: "transcription")
    static let convert = Logger(subsystem: subsystem, category: "convert")
    static let model = Logger(subsystem: subsystem, category: "model")
}

/// Human-facing progress + errors go to stderr so stdout carries only the
/// transcript text (clean for piping into another tool).
enum Stderr {
    static func log(_ message: String) {
        FileHandle.standardError.write(Data((message + "\n").utf8))
    }
}
