import Foundation

/// Decodes any input audio into a format Apple's audio stack can open.
///
/// WhatsApp voice notes are Ogg/Opus. `AVAudioFile` / `SpeechAnalyzer` cannot
/// open Ogg containers, so we shell out to the system ffmpeg to produce a
/// normalized 16 kHz mono 16-bit PCM WAV — the sample rate Apple's speech
/// models are trained at, and a codec-free container `AVAudioFile` always
/// opens. Running this for every input (not just Ogg) keeps the path uniform.
struct AudioConverter {
    let ffmpegPath: String

    /// Convert `input` to a temporary WAV. Caller owns cleanup of the returned URL.
    func convertToWav(_ input: URL) throws -> URL {
        guard FileManager.default.isExecutableFile(atPath: ffmpegPath) else {
            throw TranscribeError.ffmpegNotFound(ffmpegPath)
        }

        let output = FileManager.default.temporaryDirectory
            .appendingPathComponent("wa-transcribe-\(UUID().uuidString).wav")

        let process = Process()
        process.executableURL = URL(fileURLWithPath: ffmpegPath)
        process.arguments = [
            "-nostdin",              // never touch our stdin
            "-hide_banner",
            "-loglevel", "error",
            "-y",                    // overwrite the temp file
            "-i", input.path,
            "-ar", "16000",          // 16 kHz — the speech model's native rate
            "-ac", "1",              // mono
            "-c:a", "pcm_s16le",     // 16-bit little-endian PCM
            output.path,
        ]
        let errPipe = Pipe()
        process.standardError = errPipe
        process.standardOutput = FileHandle.nullDevice

        WALog.convert.info("ffmpeg \(input.lastPathComponent, privacy: .public) -> wav (16k mono)")
        try process.run()
        // Drain stderr before waiting so a chatty ffmpeg can't deadlock on a
        // full pipe buffer (loglevel=error keeps this tiny in practice).
        let errData = errPipe.fileHandleForReading.readDataToEndOfFile()
        process.waitUntilExit()

        guard process.terminationStatus == 0 else {
            try? FileManager.default.removeItem(at: output)
            throw TranscribeError.ffmpegFailed(
                exit: process.terminationStatus,
                stderr: String(decoding: errData, as: UTF8.self)
            )
        }
        return output
    }
}
