import Foundation

// VENDORED from Mira (`~/code/atrialballoon/apps/mira`):
//   Mira/Models/Transcript.swift
// These types are protocol-boundaried and dependency-free. Copied rather than
// linked so this CLI stays standalone. Keep in sync deliberately, not
// automatically — Mira owns the canonical version.

/// A complete transcription: an ordered list of timestamped segments.
struct Transcript: Sendable {
    let segments: [TranscriptSegment]

    var fullText: String {
        segments.map(\.text).joined(separator: " ")
    }

    var duration: TimeInterval {
        guard let last = segments.last else { return 0 }
        return last.endTime
    }

    /// Merge speech-recognition fragments into sentence-level segments.
    ///
    /// SpeechAnalyzer produces granular streaming fragments that break
    /// mid-sentence. This reconstitutes complete sentences using two heuristics:
    ///
    /// 1. **Punctuation boundary**: a fragment ending in `.`, `?`, `!`, or `:`
    ///    closes the current sentence.
    /// 2. **Pause boundary**: a gap between consecutive fragments larger than
    ///    the threshold is treated as a sentence break even without punctuation.
    func mergedSegments(pauseThreshold: TimeInterval = 1.5) -> [TranscriptSegment] {
        guard !segments.isEmpty else { return [] }

        var result: [TranscriptSegment] = []
        var currentText = ""
        var currentStart = segments[0].startTime
        var currentEnd = segments[0].endTime
        var currentConfidence: Float = 0
        var fragmentCount: Float = 0

        for (index, segment) in segments.enumerated() {
            // Check for a pause boundary before this segment.
            if index > 0 {
                let gap = segment.startTime - currentEnd
                if gap > pauseThreshold && !currentText.isEmpty {
                    result.append(TranscriptSegment(
                        text: currentText.trimmingCharacters(in: .whitespaces),
                        startTime: currentStart,
                        endTime: currentEnd,
                        confidence: currentConfidence / max(fragmentCount, 1)
                    ))
                    currentText = ""
                    currentStart = segment.startTime
                    currentConfidence = 0
                    fragmentCount = 0
                }
            }

            // Accumulate this fragment.
            if currentText.isEmpty {
                currentStart = segment.startTime
            }
            currentText += segment.text
            currentEnd = segment.endTime
            currentConfidence += segment.confidence
            fragmentCount += 1

            // Check for a punctuation boundary.
            let trimmed = segment.text.trimmingCharacters(in: .whitespacesAndNewlines)
            let endsWithPunctuation = trimmed.hasSuffix(".") || trimmed.hasSuffix("?")
                || trimmed.hasSuffix("!") || trimmed.hasSuffix(":")
            if endsWithPunctuation {
                result.append(TranscriptSegment(
                    text: currentText.trimmingCharacters(in: .whitespaces),
                    startTime: currentStart,
                    endTime: currentEnd,
                    confidence: currentConfidence / max(fragmentCount, 1)
                ))
                currentText = ""
                currentConfidence = 0
                fragmentCount = 0
            }
        }

        // Flush any remaining text.
        if !currentText.trimmingCharacters(in: .whitespaces).isEmpty {
            result.append(TranscriptSegment(
                text: currentText.trimmingCharacters(in: .whitespaces),
                startTime: currentStart,
                endTime: currentEnd,
                confidence: currentConfidence / max(fragmentCount, 1)
            ))
        }

        return result
    }
}

/// A single segment of transcribed speech with word-level timestamps.
struct TranscriptSegment: Sendable, Identifiable {
    let id: UUID
    let text: String
    let startTime: TimeInterval
    let endTime: TimeInterval
    let confidence: Float

    init(
        id: UUID = UUID(),
        text: String,
        startTime: TimeInterval,
        endTime: TimeInterval,
        confidence: Float
    ) {
        self.id = id
        self.text = text
        self.startTime = startTime
        self.endTime = endTime
        self.confidence = confidence
    }
}
