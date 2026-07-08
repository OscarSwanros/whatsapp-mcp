import Foundation

/// Every failure the CLI can surface, each with a human-readable message so the
/// tool can exit with one clear line on stderr.
enum TranscribeError: Error, CustomStringConvertible {
    case inputNotFound(String)
    case noAudioFilesInDirectory(String)
    case ffmpegNotFound(String)
    case ffmpegFailed(exit: Int32, stderr: String)
    case modelUnsupported(localeID: String)
    case modelNotInstalledOffline(localeID: String)
    case emptyTranscription
    case badArguments(String)

    var description: String {
        switch self {
        case .inputNotFound(let path):
            return "input not found: \(path)"
        case .noAudioFilesInDirectory(let path):
            return "no audio files found in directory: \(path)"
        case .ffmpegNotFound(let path):
            return "ffmpeg not found or not executable at \(path) (install with `brew install ffmpeg`, or pass --ffmpeg <path>)"
        case .ffmpegFailed(let code, let stderr):
            let detail = stderr.trimmingCharacters(in: .whitespacesAndNewlines)
            return "ffmpeg failed (exit \(code))" + (detail.isEmpty ? "" : ": \(detail)")
        case .modelUnsupported(let localeID):
            return "no on-device speech model supports locale \(localeID) (try --locale en_US or --list-locales)"
        case .modelNotInstalledOffline(let localeID):
            return "speech model for \(localeID) is not installed and --offline forbids downloading it"
        case .emptyTranscription:
            return "transcription produced no text (silent or unintelligible audio?)"
        case .badArguments(let message):
            return message
        }
    }
}
