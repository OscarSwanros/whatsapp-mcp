import AVFoundation
import OSLog
import Speech

// VENDORED from Mira (`~/code/atrialballoon/apps/mira`):
//   Mira/Services/SpeechRecognizing.swift
// The only edit is swapping Mira's `MiraLogger` for this CLI's `WALog`. The
// engine is inherently on-device: SpeechAnalyzer + SpeechTranscriber run
// locally with no network. Audio never leaves the Mac.

/// Boundary protocol for speech recognition, enabling test injection.
protocol SpeechRecognizing: Sendable {
    func recognize(
        audioSource: URL,
        locale: Locale,
        onProgress: @Sendable (Double) -> Void
    ) async throws -> [TranscriptSegment]
}

/// Production implementation wrapping Apple's SpeechAnalyzer + SpeechTranscriber
/// APIs (macOS 26+).
struct SpeechAnalyzerRecognizer: SpeechRecognizing {

    func recognize(
        audioSource: URL,
        locale: Locale,
        onProgress: @Sendable (Double) -> Void
    ) async throws -> [TranscriptSegment] {
        WALog.transcription.info("SpeechAnalyzerRecognizer: opening \(audioSource.lastPathComponent, privacy: .public)")

        // Load total duration for progress estimation.
        let asset = AVURLAsset(url: audioSource)
        let totalDuration = try await asset.load(.duration).seconds
        WALog.transcription.info("Audio duration: \(String(format: "%.1f", totalDuration))s")
        guard totalDuration > 0 else {
            WALog.transcription.warning("Audio duration is 0 — returning empty")
            return []
        }

        // Configure the transcriber with a time-indexed preset for word-level
        // timestamps (drives the sentence-merge heuristic downstream).
        let transcriber = SpeechTranscriber(
            locale: locale,
            preset: .timeIndexedTranscriptionWithAlternatives
        )

        // Create the analyzer from the audio file. `finishAfterFile: true`
        // starts processing automatically. We retain the analyzer until results
        // are fully consumed to prevent premature deallocation.
        WALog.transcription.info("Opening AVAudioFile")
        let audioFile = try AVAudioFile(forReading: audioSource)
        WALog.transcription.info("AVAudioFile opened: format \(audioFile.processingFormat.description, privacy: .public), length \(audioFile.length) frames")

        WALog.transcription.info("Creating SpeechAnalyzer")
        let analyzer = try await SpeechAnalyzer(
            inputAudioFile: audioFile,
            modules: [transcriber],
            finishAfterFile: true
        )
        WALog.transcription.info("SpeechAnalyzer created, consuming results")

        var segments: [TranscriptSegment] = []
        var resultCount = 0

        for try await result in transcriber.results {
            resultCount += 1
            guard result.isFinal else {
                WALog.transcription.debug("Partial result #\(resultCount) skipped")
                continue
            }

            let startSeconds = result.range.start.seconds
            let endSeconds = result.range.end.seconds
            let text = String(result.text.characters)

            WALog.transcription.debug("Segment: [\(String(format: "%.1f", startSeconds))-\(String(format: "%.1f", endSeconds))s] \(text.prefix(80), privacy: .private)")

            let segment = TranscriptSegment(
                text: text,
                startTime: startSeconds,
                endTime: endSeconds,
                confidence: 1.0 // SpeechTranscriber.Result does not expose per-segment confidence.
            )
            segments.append(segment)

            let progress = min(endSeconds / totalDuration, 1.0)
            onProgress(progress)
        }

        // Keep the analyzer alive until results are fully consumed.
        _ = analyzer

        WALog.transcription.info("Recognition done: \(segments.count) final segment(s) from \(resultCount) total result(s)")
        onProgress(1.0)
        return segments
    }
}
