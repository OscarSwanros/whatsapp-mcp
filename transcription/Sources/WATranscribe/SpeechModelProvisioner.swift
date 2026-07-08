import Foundation
import Speech

/// Resolves a requested locale to an installed, ready-to-use on-device speech
/// model — downloading it once if needed and permitted.
///
/// Adapted from Mira's `TranscriptionService.resolveLocale` (locale fallback)
/// and `PermissionService.downloadSpeechModel` (the `AssetInventory` install
/// request). The model download is the ONLY network touch in this whole tool;
/// once installed, transcription is entirely on-device.
struct SpeechModelProvisioner {
    /// When false (`--offline`), refuse to hit the network: a missing model is
    /// a hard error rather than a download.
    let allowDownload: Bool

    /// Return a locale whose model is installed and ready. Tries the preferred
    /// locale, then its regional and bare-language fallbacks
    /// (e.g. es_MX → es_ES → es).
    func provision(preferred: Locale) async throws -> Locale {
        for candidate in Self.fallbacks(for: preferred) {
            let transcriber = SpeechTranscriber(
                locale: candidate,
                preset: .timeIndexedTranscriptionWithAlternatives
            )
            let status = await AssetInventory.status(forModules: [transcriber])
            WALog.model.info("model status for \(candidate.identifier, privacy: .public): \(String(describing: status), privacy: .public)")

            switch status {
            case .installed:
                return candidate

            case .supported, .downloading:
                // Available for this locale but not yet on disk. Installing is
                // idempotent and also awaits an in-flight download to finish.
                guard allowDownload else {
                    throw TranscribeError.modelNotInstalledOffline(localeID: candidate.identifier)
                }
                try await download(transcriber, locale: candidate)
                return candidate

            case .unsupported:
                continue

            @unknown default:
                continue
            }
        }
        throw TranscribeError.modelUnsupported(localeID: preferred.identifier)
    }

    private func download(_ transcriber: SpeechTranscriber, locale: Locale) async throws {
        Stderr.log("Downloading on-device speech model for \(locale.identifier) (one-time, requires network)…")
        WALog.model.info("requesting model download for \(locale.identifier, privacy: .public)")

        guard let request = try await AssetInventory.assetInstallationRequest(supporting: [transcriber]) else {
            // Nothing left to install — treat as ready.
            return
        }
        try await request.downloadAndInstall()
        Stderr.log("Model ready.")
    }

    /// Build a locale fallback list: exact → common region → bare language.
    /// Mirrors Mira's `localeFallbacks`.
    static func fallbacks(for locale: Locale) -> [Locale] {
        var candidates: [Locale] = [locale]
        let lang = locale.language.languageCode?.identifier ?? "en"

        let commonRegions: [String: String] = [
            "en": "US", "es": "ES", "fr": "FR", "de": "DE",
            "pt": "BR", "ja": "JP", "zh": "CN", "ko": "KR",
            "it": "IT", "nl": "NL", "ru": "RU",
        ]
        if let region = commonRegions[lang] {
            let regionLocale = Locale(identifier: "\(lang)_\(region)")
            if !candidates.contains(where: { $0.identifier == regionLocale.identifier }) {
                candidates.append(regionLocale)
            }
        }

        let bareLocale = Locale(identifier: lang)
        if !candidates.contains(where: { $0.identifier == bareLocale.identifier }) {
            candidates.append(bareLocale)
        }

        return candidates
    }
}
