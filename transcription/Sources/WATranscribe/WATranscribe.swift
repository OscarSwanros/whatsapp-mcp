import Foundation
import Speech

@main
struct WATranscribe {
    static func main() async {
        do {
            let options = try Options.parse(Array(CommandLine.arguments.dropFirst()))

            if options.showHelp {
                print(Options.helpText)
                return
            }
            if options.listLocales {
                await printSupportedLocales()
                return
            }

            let ok = try await Runner(options: options).run()
            if !ok { exit(1) }
        } catch let error as TranscribeError {
            Stderr.log("error: \(error.description)")
            exit(1)
        } catch {
            Stderr.log("error: \(error.localizedDescription)")
            exit(1)
        }
    }

    private static func printSupportedLocales() async {
        let locales = await SpeechTranscriber.supportedLocales
        for id in locales.map(\.identifier).sorted() {
            print(id)
        }
    }
}

// MARK: - Options

struct Options {
    var inputPath: String = ""
    var localeID: String = "es_MX"   // Mexican Spanish, Oscar's dominant register.
    var ffmpegPath: String = "/opt/homebrew/bin/ffmpeg"
    var writeTxt: Bool = false
    var offline: Bool = false
    var keepTemp: Bool = false
    var quiet: Bool = false
    var showHelp: Bool = false
    var listLocales: Bool = false

    /// Audio extensions recognized when the input is a directory.
    static let audioExtensions: Set<String> = [
        "ogg", "opus", "oga", "m4a", "mp3", "wav", "caf", "aac", "aif", "aiff", "flac", "mp4",
    ]

    static func parse(_ args: [String]) throws -> Options {
        var options = Options()
        var index = 0

        func value(for flag: String) throws -> String {
            index += 1
            guard index < args.count else {
                throw TranscribeError.badArguments("\(flag) requires a value")
            }
            return args[index]
        }

        while index < args.count {
            let arg = args[index]
            switch arg {
            case "-h", "--help":
                options.showHelp = true
            case "--list-locales":
                options.listLocales = true
            case "--locale", "-l":
                options.localeID = try value(for: arg)
            case "--ffmpeg":
                options.ffmpegPath = try value(for: arg)
            case "--txt":
                options.writeTxt = true
            case "--offline":
                options.offline = true
            case "--keep-temp":
                options.keepTemp = true
            case "--quiet", "-q":
                options.quiet = true
            default:
                if arg.hasPrefix("-") {
                    throw TranscribeError.badArguments("unknown option: \(arg)")
                }
                if options.inputPath.isEmpty {
                    options.inputPath = arg
                } else {
                    throw TranscribeError.badArguments("unexpected extra argument: \(arg)")
                }
            }
            index += 1
        }

        if !options.showHelp && !options.listLocales && options.inputPath.isEmpty {
            throw TranscribeError.badArguments("missing input path (a .ogg voice note or a directory). See --help.")
        }
        return options
    }

    static let helpText = """
    wa-transcribe — on-device WhatsApp voice-note transcription (macOS 26+)

    USAGE:
      wa-transcribe <file-or-directory> [options]

    ARGUMENTS:
      <file-or-directory>   A voice note (.ogg / .opus / …) or a directory of them.

    OPTIONS:
      -l, --locale <id>     Speech locale (default: es_MX). Falls back
                            es_MX → es_ES → es. Use --list-locales to see options.
          --txt             Also write a sibling <name>.txt next to each input.
          --offline         Refuse to download a missing model (fail instead).
          --ffmpeg <path>   ffmpeg binary (default: /opt/homebrew/bin/ffmpeg).
          --keep-temp       Keep the intermediate WAV (debugging).
      -q, --quiet           Suppress progress on stderr.
          --list-locales    Print installed/supported speech locales and exit.
      -h, --help            Show this help.

    OUTPUT:
      The transcript is printed to stdout. With multiple inputs, each is
      preceded by a `=== <filename> ===` header. Diagnostics go to stderr, so
      `wa-transcribe note.ogg > out.txt` captures a clean transcript.

    PRIVACY:
      Transcription is entirely on-device (Apple SpeechAnalyzer). Audio never
      leaves the Mac. The only network access is a one-time speech-model
      download (suppressible with --offline once the model is installed).
    """
}

// MARK: - Runner

struct Runner {
    let options: Options

    func run() async throws -> Bool {
        let inputs = try resolveInputs()

        let converter = AudioConverter(ffmpegPath: options.ffmpegPath)
        let provisioner = SpeechModelProvisioner(allowDownload: !options.offline)
        let recognizer = SpeechAnalyzerRecognizer()

        // Resolve the locale/model once, up front, shared across every file.
        let locale = try await provisioner.provision(preferred: Locale(identifier: options.localeID))
        if locale.identifier != options.localeID {
            Stderr.log("Using locale \(locale.identifier) (requested \(options.localeID)).")
        }

        let multiple = inputs.count > 1
        var allOK = true

        for input in inputs {
            do {
                let text = try await transcribeOne(
                    input,
                    converter: converter,
                    recognizer: recognizer,
                    locale: locale
                )
                if multiple { print("=== \(input.lastPathComponent) ===") }
                print(text)
                if multiple { print("") }

                if options.writeTxt {
                    let txtURL = input.deletingPathExtension().appendingPathExtension("txt")
                    try text.write(to: txtURL, atomically: true, encoding: .utf8)
                    if !options.quiet { Stderr.log("Wrote \(txtURL.path)") }
                }
            } catch {
                allOK = false
                let message = (error as? TranscribeError)?.description ?? error.localizedDescription
                Stderr.log("error transcribing \(input.lastPathComponent): \(message)")
            }
        }
        return allOK
    }

    private func transcribeOne(
        _ input: URL,
        converter: AudioConverter,
        recognizer: SpeechAnalyzerRecognizer,
        locale: Locale
    ) async throws -> String {
        if !options.quiet { Stderr.log("Converting \(input.lastPathComponent)…") }
        let wav = try converter.convertToWav(input)
        defer {
            if !options.keepTemp { try? FileManager.default.removeItem(at: wav) }
        }

        if !options.quiet { Stderr.log("Transcribing on-device (\(locale.identifier))…") }
        let segments = try await recognizer.recognize(
            audioSource: wav,
            locale: locale,
            onProgress: { _ in }
        )
        guard !segments.isEmpty else { throw TranscribeError.emptyTranscription }

        let transcript = Transcript(segments: segments)
        let merged = transcript.mergedSegments()
        let joined = merged.map(\.text).joined(separator: " ")
        let text = Self.normalizeWhitespace(joined.isEmpty ? transcript.fullText : joined)
        guard !text.isEmpty else { throw TranscribeError.emptyTranscription }
        return text
    }

    private func resolveInputs() throws -> [URL] {
        let fm = FileManager.default
        var isDirectory: ObjCBool = false
        guard fm.fileExists(atPath: options.inputPath, isDirectory: &isDirectory) else {
            throw TranscribeError.inputNotFound(options.inputPath)
        }

        if isDirectory.boolValue {
            let dir = URL(fileURLWithPath: options.inputPath)
            let entries = try fm.contentsOfDirectory(at: dir, includingPropertiesForKeys: nil)
            let audio = entries
                .filter { Options.audioExtensions.contains($0.pathExtension.lowercased()) }
                .sorted { $0.lastPathComponent < $1.lastPathComponent }
            guard !audio.isEmpty else {
                throw TranscribeError.noAudioFilesInDirectory(options.inputPath)
            }
            return audio
        }

        return [URL(fileURLWithPath: options.inputPath)]
    }

    /// Collapse runs of whitespace to single spaces and trim the ends. Streaming
    /// speech fragments can carry leading/trailing spaces that double up when
    /// joined.
    static func normalizeWhitespace(_ text: String) -> String {
        let collapsed = text
            .split(whereSeparator: { $0 == " " || $0 == "\n" || $0 == "\t" })
            .joined(separator: " ")
        return collapsed.trimmingCharacters(in: .whitespacesAndNewlines)
    }
}
