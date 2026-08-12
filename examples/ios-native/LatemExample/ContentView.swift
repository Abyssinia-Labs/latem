import SwiftUI

/// The app deliberately does almost nothing. It exists so `latem` has a real
/// iOS target to build, archive, sign, and upload — the point under test is the
/// pipeline, not this view.
///
/// It does display the build number, because that is the one thing worth seeing
/// on a device: it tells you *which* build reached TestFlight, which is the
/// question you actually have when a run finishes.
struct ContentView: View {
  private var version: String {
    let dict = Bundle.main.infoDictionary
    let short = dict?["CFBundleShortVersionString"] as? String ?? "?"
    let build = dict?["CFBundleVersion"] as? String ?? "?"
    return "\(short) (\(build))"
  }

  var body: some View {
    VStack(spacing: 12) {
      Image(systemName: "hammer.fill")
        .font(.system(size: 48))
        .foregroundStyle(.tint)
      Text("Latem")
        .font(.largeTitle.bold())
      Text("build \(version)")
        .font(.footnote.monospaced())
        .foregroundStyle(.secondary)
    }
    .padding()
  }
}

#Preview {
  ContentView()
}
