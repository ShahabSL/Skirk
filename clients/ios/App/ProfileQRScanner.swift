import AVFoundation
import SwiftUI
import VisionKit

struct ProfileQRScanner: View {
    let onScanned: (String) -> Void

    private enum CameraState { case checking, scanning, denied, unavailable }

    @Environment(\.dismiss) private var dismiss
    @Environment(\.openURL) private var openURL
    @Environment(\.scenePhase) private var scenePhase
    @State private var cameraState: CameraState = .checking
    @State private var scannedOtherCode = false

    var body: some View {
        NavigationStack {
            Group {
                switch cameraState {
                case .checking:
                    ProgressView("Opening camera…")
                case .scanning:
                    if scenePhase == .active {
                        VStack(spacing: 0) {
                            CameraPreview { result in
                                switch result {
                                case .profile(let profile): onScanned(profile)
                                case .otherCode: scannedOtherCode = true
                                case .unavailable: cameraState = .unavailable
                                }
                            }
                            .privacySensitive()
                            Text(scannedOtherCode
                                 ? "Use the Skirk profile QR code from your exit."
                                 : "Point your camera at the QR code from your Skirk exit.")
                                .font(.body)
                                .multilineTextAlignment(.center)
                                .padding()
                        }
                    }
                case .denied:
                    ContentUnavailableView {
                        Label("Camera access is off", systemImage: "camera")
                    } description: {
                        Text("Allow camera access in Settings to scan a QR code, or close this sheet to paste a profile.")
                    } actions: {
                        Button("Open Settings") {
                            if let url = URL(string: UIApplication.openSettingsURLString) {
                                openURL(url)
                            }
                        }
                    }
                case .unavailable:
                    ContentUnavailableView(
                        "Camera unavailable",
                        systemImage: "camera",
                        description: Text("Close this sheet to paste a profile or choose a profile file.")
                    )
                }
            }
            .navigationTitle("Scan QR code")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") { dismiss() }
                }
            }
            .task { await checkCamera() }
            .onChange(of: scenePhase) { _, phase in
                if phase == .active && cameraState == .denied &&
                    AVCaptureDevice.authorizationStatus(for: .video) == .authorized {
                    cameraState = DataScannerViewController.isAvailable ? .scanning : .unavailable
                }
            }
        }
    }

    private func checkCamera() async {
        guard DataScannerViewController.isSupported, AVCaptureDevice.default(for: .video) != nil else {
            cameraState = .unavailable
            return
        }

        switch AVCaptureDevice.authorizationStatus(for: .video) {
        case .notDetermined:
            let granted = await AVCaptureDevice.requestAccess(for: .video)
            guard !Task.isCancelled else { return }
            cameraState = granted ? .scanning : .denied
        case .authorized:
            cameraState = .scanning
        case .denied:
            cameraState = .denied
        case .restricted:
            cameraState = .unavailable
        @unknown default:
            cameraState = .unavailable
        }
        if cameraState == .scanning && !DataScannerViewController.isAvailable {
            cameraState = .unavailable
        }
    }
}

private struct CameraPreview: UIViewControllerRepresentable {
    enum Result { case profile(String), otherCode, unavailable }

    let onResult: (Result) -> Void

    func makeUIViewController(context: Context) -> Controller {
        Controller(onResult: onResult)
    }

    func updateUIViewController(_ controller: Controller, context: Context) {}

    final class Controller: UIViewController, DataScannerViewControllerDelegate {
        private let scanner = DataScannerViewController(
            recognizedDataTypes: [.barcode(symbologies: [.qr])],
            qualityLevel: .accurate,
            isHighFrameRateTrackingEnabled: false
        )
        private let onResult: (Result) -> Void
        private var finished = false

        init(onResult: @escaping (Result) -> Void) {
            self.onResult = onResult
            super.init(nibName: nil, bundle: nil)
        }

        @available(*, unavailable)
        required init?(coder: NSCoder) { fatalError("Use programmatic initialization") }

        override func viewDidLoad() {
            super.viewDidLoad()
            scanner.delegate = self
            addChild(scanner)
            scanner.view.frame = view.bounds
            scanner.view.autoresizingMask = [.flexibleWidth, .flexibleHeight]
            view.addSubview(scanner.view)
            scanner.didMove(toParent: self)
        }

        override func viewDidAppear(_ animated: Bool) {
            super.viewDidAppear(animated)
            guard !finished else { return }
            do {
                try scanner.startScanning()
            } catch {
                finish(.unavailable)
            }
        }

        override func viewWillDisappear(_ animated: Bool) {
            super.viewWillDisappear(animated)
            scanner.stopScanning()
        }

        func dataScanner(_ dataScanner: DataScannerViewController, didAdd addedItems: [RecognizedItem], allItems: [RecognizedItem]) {
            guard !finished, dataScanner.isScanning else { return }
            for item in addedItems {
                guard case .barcode(let barcode) = item, let payload = barcode.payloadStringValue else { continue }
                let profile = payload.trimmingCharacters(in: .whitespacesAndNewlines)
                if profile.hasPrefix("skirk:") {
                    finish(.profile(profile))
                    return
                }
                onResult(.otherCode)
            }
        }

        func dataScanner(_ dataScanner: DataScannerViewController, becameUnavailableWithError error: DataScannerViewController.ScanningUnavailable) {
            finish(.unavailable)
        }

        private func finish(_ result: Result) {
            guard !finished else { return }
            finished = true
            scanner.stopScanning()
            onResult(result)
        }
    }
}
