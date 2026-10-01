// DynApp macOS app host (docs/native-host.md in the dynapp repository).
//
// One copy of this binary lives in every installed app bundle. It owns the
// window, the WKWebView, navigation policy, the permission prompt, and the
// private connection to the DynApp agent. Everything else, including the page
// bridge script, comes from the agent so this binary rarely changes: a new
// binary resets the macOS privacy grants of every installed app.

import Cocoa
import Darwin
import UniformTypeIdentifiers
import WebKit

struct HostSettings {
  let storeID: String
  let origin: URL
  let startURL: URL
  let socketPath: String
  let appName: String

  static func load() -> HostSettings? {
    let info = Bundle.main.infoDictionary ?? [:]
    guard let storeID = info["DynAppStoreID"] as? String,
      let originText = info["DynAppOrigin"] as? String, let origin = URL(string: originText),
      let socketPath = info["DynAppAgentSocket"] as? String
    else { return nil }
    let start = (info["DynAppURL"] as? String).flatMap(URL.init(string:)) ?? origin
    let name = (info["CFBundleDisplayName"] as? String) ?? (info["CFBundleName"] as? String) ?? "DynApp"
    return HostSettings(storeID: storeID, origin: origin, startURL: start, socketPath: socketPath, appName: name)
  }
}

func originKey(_ url: URL?) -> String {
  guard let url, let scheme = url.scheme?.lowercased(), let host = url.host?.lowercased() else { return "" }
  if let port = url.port, !(scheme == "https" && port == 443) && !(scheme == "http" && port == 80) {
    return "\(scheme)://\(host):\(port)"
  }
  return "\(scheme)://\(host)"
}

func originKey(_ origin: WKSecurityOrigin) -> String {
  let scheme = origin.protocol.lowercased()
  let host = origin.host.lowercased()
  if origin.port != 0 && !(scheme == "https" && origin.port == 443) && !(scheme == "http" && origin.port == 80) {
    return "\(scheme)://\(host):\(origin.port)"
  }
  return "\(scheme)://\(host)"
}

// MARK: - Agent connection

/// One stream to the agent's native-host socket. Frames are
/// `uint32 length | uint8 kind | payload`, where length counts kind + payload.
final class AgentConnection {
  static let text: UInt8 = 1
  static let binary: UInt8 = 2

  private let fd: Int32
  private let writeLock = NSLock()
  private var closed = false
  var onFrame: ((UInt8, Data) -> Void)?
  var onClose: (() -> Void)?

  init?(path: String) {
    let descriptor = socket(AF_UNIX, SOCK_STREAM, 0)
    guard descriptor >= 0 else { return nil }
    var one: Int32 = 1
    setsockopt(descriptor, SOL_SOCKET, SO_NOSIGPIPE, &one, socklen_t(MemoryLayout<Int32>.size))
    var address = sockaddr_un()
    address.sun_family = sa_family_t(AF_UNIX)
    let bytes = Array(path.utf8)
    let capacity = MemoryLayout.size(ofValue: address.sun_path)
    guard bytes.count < capacity else {
      Darwin.close(descriptor)
      return nil
    }
    withUnsafeMutableBytes(of: &address.sun_path) { buffer in
      for (index, byte) in bytes.enumerated() { buffer[index] = byte }
    }
    let result = withUnsafePointer(to: &address) {
      $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
        Darwin.connect(descriptor, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
      }
    }
    guard result == 0 else {
      Darwin.close(descriptor)
      return nil
    }
    fd = descriptor
  }

  func start() {
    let thread = Thread { [weak self] in self?.readLoop() }
    thread.stackSize = 1 << 20
    thread.start()
  }

  private func readExactly(_ count: Int) -> Data? {
    var data = Data(count: count)
    var offset = 0
    while offset < count {
      let read = data.withUnsafeMutableBytes { buffer -> Int in
        Darwin.read(fd, buffer.baseAddress!.advanced(by: offset), count - offset)
      }
      if read <= 0 { return nil }
      offset += read
    }
    return data
  }

  private func readLoop() {
    while true {
      guard let header = readExactly(5) else { break }
      let length = header.prefix(4).reduce(UInt32(0)) { ($0 << 8) | UInt32($1) }
      guard length >= 1, length <= 64 * 1024 * 1024 else { break }
      let kind = header[header.startIndex + 4]
      guard let payload = length > 1 ? readExactly(Int(length) - 1) : Data() else { break }
      onFrame?(kind, payload)
    }
    close()
    onClose?()
  }

  @discardableResult
  func send(kind: UInt8, payload: Data) -> Bool {
    let length = UInt32(payload.count + 1)
    var frame = Data([UInt8(length >> 24), UInt8((length >> 16) & 0xff), UInt8((length >> 8) & 0xff), UInt8(length & 0xff), kind])
    frame.append(payload)
    writeLock.lock()
    defer { writeLock.unlock() }
    if closed { return false }
    return frame.withUnsafeBytes { buffer -> Bool in
      var offset = 0
      while offset < buffer.count {
        let written = Darwin.write(fd, buffer.baseAddress!.advanced(by: offset), buffer.count - offset)
        if written <= 0 { return false }
        offset += written
      }
      return true
    }
  }

  @discardableResult
  func sendJSON(_ object: [String: Any]) -> Bool {
    guard let data = try? JSONSerialization.data(withJSONObject: object) else { return false }
    return send(kind: AgentConnection.text, payload: data)
  }

  func close() {
    writeLock.lock()
    defer { writeLock.unlock() }
    if closed { return }
    closed = true
    Darwin.shutdown(fd, SHUT_RDWR)
    Darwin.close(fd)
  }
}

func decodeJSON(_ data: Data) -> [String: Any]? {
  (try? JSONSerialization.jsonObject(with: data)) as? [String: Any]
}

// MARK: - Page socket

/// Bridges one page `NativeAgentSocket` to one agent connection.
final class PageSocket {
  let sid: String
  let connection: AgentConnection
  var ready = false
  weak var host: HostController?

  init(sid: String, connection: AgentConnection, host: HostController) {
    self.sid = sid
    self.connection = connection
    self.host = host
  }

  func open(storeID: String) {
    connection.onFrame = { [weak self] kind, payload in
      DispatchQueue.main.async { self?.received(kind: kind, payload: payload) }
    }
    connection.onClose = { [weak self] in
      DispatchQueue.main.async {
        guard let self else { return }
        self.host?.socketEnded(self, code: 1006, reason: "The DynApp agent connection closed")
      }
    }
    connection.start()
    connection.sendJSON(["type": "native-host-hello", "version": 1, "storeId": storeID, "purpose": "app"])
  }

  private func received(kind: UInt8, payload: Data) {
    guard let host else { return }
    if !ready || kind == AgentConnection.text, let frame = (kind == AgentConnection.text ? decodeJSON(payload) : nil),
      let type = frame["type"] as? String, type.hasPrefix("native-host-")
    {
      switch type {
      case "native-host-ready":
        ready = true
        host.deliver(["sid": sid, "event": "open"])
      case "native-host-permission-request":
        host.presentPermissionRequest(frame) { [weak self] capabilities in
          self?.connection.sendJSON([
            "type": "native-host-permission-decision", "requestId": frame["requestId"] ?? "",
            "capabilities": capabilities.map { $0 as Any } ?? NSNull(),
          ])
        }
      case "native-host-error":
        host.deliver(["sid": sid, "event": "error", "error": frame["error"] ?? "The DynApp agent refused the connection"])
        host.socketEnded(self, code: 4401, reason: (frame["error"] as? String) ?? "Rejected")
      case "native-host-close":
        host.socketEnded(self, code: (frame["code"] as? Int) ?? 1000, reason: (frame["reason"] as? String) ?? "")
      default:
        break
      }
      return
    }
    guard ready else { return }
    if kind == AgentConnection.binary {
      host.deliver(["sid": sid, "event": "message", "binary": payload.base64EncodedString()])
    } else {
      host.deliver(["sid": sid, "event": "message", "text": String(decoding: payload, as: UTF8.self)])
    }
  }
}

// MARK: - Web view

/// Reports native file drops with their real paths, then lets WebKit run the
/// normal DOM drop.
final class HostWebView: WKWebView {
  var onFileDrop: (([String], CGPoint) -> Void)?

  override func performDragOperation(_ sender: NSDraggingInfo) -> Bool {
    let options: [NSPasteboard.ReadingOptionKey: Any] = [.urlReadingFileURLsOnly: true]
    let urls = sender.draggingPasteboard.readObjects(forClasses: [NSURL.self], options: options) as? [URL] ?? []
    let local = convert(sender.draggingLocation, from: nil)
    let point = CGPoint(x: local.x, y: isFlipped ? local.y : bounds.height - local.y)
    let handled = super.performDragOperation(sender)
    if !urls.isEmpty { onFileDrop?(urls.map(\.path), point) }
    return handled
  }
}

// MARK: - Host controller

final class HostController: NSObject, NSApplicationDelegate, WKScriptMessageHandler, WKNavigationDelegate,
  WKUIDelegate, WKDownloadDelegate, NSWindowDelegate
{
  let settings: HostSettings
  var window: NSWindow!
  var webView: HostWebView!
  var control: AgentConnection?
  var agentOrigin = ""
  var sockets: [String: PageSocket] = [:]
  var popups: [NSWindow] = []
  var pendingReviews: [String: String] = [:]
  var permissionQueue: [(frame: [String: Any], reply: ([String]?) -> Void)] = []
  var shownRequests = Set<String>()
  var permissionSheetOpen = false

  init(settings: HostSettings) {
    self.settings = settings
  }

  func applicationDidFinishLaunching(_ notification: Notification) {
    buildMenu()
    window = NSWindow(
      contentRect: NSRect(x: 0, y: 0, width: 1280, height: 820),
      styleMask: [.titled, .closable, .miniaturizable, .resizable], backing: .buffered, defer: false)
    window.title = settings.appName
    window.minSize = NSSize(width: 480, height: 320)
    window.delegate = self
    window.setFrameAutosaveName("DynAppMainWindow")
    if !window.setFrameUsingName("DynAppMainWindow") { window.center() }
    window.makeKeyAndOrderFront(nil)
    NSApp.activate(ignoringOtherApps: true)
    connectControl(attempt: 0)
  }

  func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { true }

  func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
    window?.makeKeyAndOrderFront(nil)
    return true
  }

  /// The control connection supplies the bridge script and start URL. Without
  /// an agent the app still loads as a plain web app.
  func connectControl(attempt: Int) {
    guard let connection = AgentConnection(path: settings.socketPath) else {
      if attempt == 0 { kickstartAgent() }
      if attempt < 12 {
        DispatchQueue.main.asyncAfter(deadline: .now() + 0.25) { self.connectControl(attempt: attempt + 1) }
      } else {
        loadPage(bootstrap: nil, url: settings.startURL)
      }
      return
    }
    var loaded = false
    let timeout = DispatchWorkItem { [weak self] in
      guard let self, !loaded else { return }
      loaded = true
      connection.close()
      self.loadPage(bootstrap: nil, url: self.settings.startURL)
    }
    DispatchQueue.main.asyncAfter(deadline: .now() + 6, execute: timeout)
    connection.onFrame = { [weak self] kind, payload in
      guard kind == AgentConnection.text, let frame = decodeJSON(payload) else { return }
      DispatchQueue.main.async {
        guard let self else { return }
        switch frame["type"] as? String {
        case "native-host-ready":
          guard !loaded else { return }
          loaded = true
          timeout.cancel()
          self.control = connection
          let url = (frame["url"] as? String).flatMap(URL.init(string:)) ?? self.settings.startURL
          self.agentOrigin = (frame["origin"] as? String) ?? ""
          self.loadPage(bootstrap: frame["bootstrapScript"] as? String, url: url)
        case "native-host-permission-request":
          self.presentPermissionRequest(frame) { capabilities in
            connection.sendJSON([
              "type": "native-host-permission-decision", "requestId": frame["requestId"] ?? "",
              "capabilities": capabilities.map { $0 as Any } ?? NSNull(),
            ])
          }
        case "native-host-review-result":
          self.finishReview(frame)
        case "native-host-error":
          guard !loaded else { return }
          loaded = true
          timeout.cancel()
          self.showError(frame["error"] as? String ?? "The DynApp agent refused this app.")
          self.loadPage(bootstrap: nil, url: self.settings.startURL)
        default:
          break
        }
      }
    }
    connection.onClose = { [weak self] in
      DispatchQueue.main.async {
        guard let self else { return }
        if self.control === connection { self.control = nil }
        for (rid, _) in self.pendingReviews {
          self.deliver(["rid": rid, "event": "review", "error": "The DynApp agent is not running"])
        }
        self.pendingReviews.removeAll()
      }
    }
    connection.start()
    connection.sendJSON(["type": "native-host-hello", "version": 1, "storeId": settings.storeID, "purpose": "control"])
  }

  func kickstartAgent() {
    let process = Process()
    process.executableURL = URL(fileURLWithPath: "/bin/launchctl")
    process.arguments = ["kickstart", "gui/\(getuid())/dynapp-shell-agent"]
    try? process.run()
  }

  func loadPage(bootstrap: String?, url: URL) {
    let configuration = WKWebViewConfiguration()
    let content = WKUserContentController()
    if let bootstrap, !bootstrap.isEmpty {
      content.add(self, contentWorld: .page, name: "dynappNativeHost")
      content.addUserScript(WKUserScript(source: bootstrap, injectionTime: .atDocumentStart, forMainFrameOnly: true, in: .page))
    }
    configuration.userContentController = content
    configuration.preferences.javaScriptCanOpenWindowsAutomatically = true
    configuration.preferences.isElementFullscreenEnabled = true
    configuration.mediaTypesRequiringUserActionForPlayback = []
    configuration.applicationNameForUserAgent = "Version/18.0 Safari/605.1.15 DynApp/1"
    webView = HostWebView(frame: window.contentView?.bounds ?? .zero, configuration: configuration)
    webView.autoresizingMask = [.width, .height]
    webView.navigationDelegate = self
    webView.uiDelegate = self
    webView.allowsBackForwardNavigationGestures = true
    if #available(macOS 13.3, *) { webView.isInspectable = true }
    webView.onFileDrop = { [weak self] paths, point in
      self?.deliver(["event": "drop", "paths": paths, "x": point.x, "y": point.y])
    }
    window.contentView = webView
    webView.load(URLRequest(url: url))
  }

  // MARK: Page bridge

  func userContentController(_ controller: WKUserContentController, didReceive message: WKScriptMessage) {
    guard message.frameInfo.isMainFrame, message.webView === webView,
      originKey(message.frameInfo.securityOrigin) == originKey(settings.origin),
      let body = message.body as? [String: Any], (body["dynappNativeHost"] as? Int) == 1,
      let op = body["op"] as? String
    else { return }
    switch op {
    case "open":
      guard let sid = body["sid"] as? String, sockets[sid] == nil else { return }
      guard let connection = AgentConnection(path: settings.socketPath) else {
        deliver(["sid": sid, "event": "error", "error": "The DynApp agent is not running"])
        deliver(["sid": sid, "event": "close", "code": 1006, "reason": "The DynApp agent is not running"])
        return
      }
      let socket = PageSocket(sid: sid, connection: connection, host: self)
      sockets[sid] = socket
      socket.open(storeID: settings.storeID)
    case "send":
      guard let sid = body["sid"] as? String, let socket = sockets[sid], socket.ready else { return }
      if let text = body["text"] as? String {
        socket.connection.send(kind: AgentConnection.text, payload: Data(text.utf8))
      } else if let encoded = body["binary"] as? String, let data = Data(base64Encoded: encoded) {
        socket.connection.send(kind: AgentConnection.binary, payload: data)
      }
    case "close":
      guard let sid = body["sid"] as? String, let socket = sockets[sid] else { return }
      socketEnded(socket, code: (body["code"] as? Int) ?? 1000, reason: (body["reason"] as? String) ?? "")
    case "review":
      guard let rid = body["rid"] as? String else { return }
      requestReview(rid: rid)
    default:
      break
    }
  }

  func socketEnded(_ socket: PageSocket, code: Int, reason: String) {
    guard sockets[socket.sid] === socket else { return }
    sockets[socket.sid] = nil
    socket.connection.close()
    deliver(["sid": socket.sid, "event": "close", "code": code, "reason": reason])
  }

  func deliver(_ message: [String: Any]) {
    guard let webView else { return }
    webView.callAsyncJavaScript(
      "window.__dynappNativeHostDeliver && window.__dynappNativeHostDeliver(message)",
      arguments: ["message": message], in: nil, in: .page, completionHandler: nil)
  }

  // MARK: Permissions

  @objc func reviewPermissions(_ sender: Any?) {
    requestReview(rid: nil)
  }

  func requestReview(rid: String?) {
    guard let control else {
      if let rid { deliver(["rid": rid, "event": "review", "error": "The DynApp agent is not running"]) }
      showError("The DynApp agent is not running, so permissions cannot be changed right now.")
      return
    }
    let id = rid ?? "menu-\(UUID().uuidString)"
    pendingReviews[id] = id
    control.sendJSON(["type": "native-host-review", "id": id])
  }

  func finishReview(_ frame: [String: Any]) {
    guard let id = frame["id"] as? String, pendingReviews.removeValue(forKey: id) != nil else { return }
    let changed = (frame["changed"] as? Bool) ?? false
    if !id.hasPrefix("menu-") {
      var reply: [String: Any] = ["rid": id, "event": "review", "changed": changed]
      if let error = frame["error"] as? String { reply["error"] = error }
      deliver(reply)
    } else if let error = frame["error"] as? String {
      showError(error)
    }
    if changed { webView?.reload() }
  }

  func presentPermissionRequest(_ frame: [String: Any], reply: @escaping ([String]?) -> Void) {
    guard let requestID = frame["requestId"] as? String, !shownRequests.contains(requestID) else { return }
    shownRequests.insert(requestID)
    permissionQueue.append((frame, reply))
    showNextPermissionRequest()
  }

  func showNextPermissionRequest() {
    guard !permissionSheetOpen, !permissionQueue.isEmpty, let window else { return }
    let (frame, reply) = permissionQueue.removeFirst()
    permissionSheetOpen = true
    let kind = (frame["kind"] as? String) ?? "pairing"
    let groups = (frame["groups"] as? [[String: Any]]) ?? []
    let appName = (frame["appName"] as? String) ?? settings.appName

    let alert = NSAlert()
    alert.alertStyle = .warning
    switch kind {
    case "review":
      alert.messageText = "Permissions for \(appName)"
      alert.informativeText = "Choose what \(appName) may do on this Mac."
    case "delta":
      alert.messageText = "\(appName) wants more access"
      alert.informativeText = "This version of \(appName) asks for new permissions. You can change this later from \(appName) ▸ Permissions…"
    default:
      alert.messageText = "Allow \(appName) to use this Mac?"
      alert.informativeText = "Choose what \(appName) may do. You can change this later from \(appName) ▸ Permissions…"
    }

    let stack = NSStackView()
    stack.orientation = .vertical
    stack.alignment = .leading
    stack.spacing = 10
    var checkboxes: [(NSButton, [String])] = []
    for group in groups {
      let danger = (group["danger"] as? String) ?? "medium"
      let title = (group["title"] as? String) ?? (group["id"] as? String) ?? "Permission"
      let checkbox = NSButton(checkboxWithTitle: "\(title) (\(danger) risk)", target: nil, action: nil)
      let granted = (group["granted"] as? Bool) ?? false
      let suggested = (group["suggested"] as? Bool) ?? false
      checkbox.state = (kind == "review" ? granted : suggested && danger != "very-high") ? .on : .off
      checkbox.font = .boldSystemFont(ofSize: NSFont.systemFontSize)
      stack.addArrangedSubview(checkbox)
      var lines: [String] = []
      if let summary = group["summary"] as? String, !summary.isEmpty { lines.append(summary) }
      for reason in (group["reasons"] as? [String]) ?? [] { lines.append("“\(reason)”") }
      if !lines.isEmpty {
        let label = NSTextField(wrappingLabelWithString: lines.joined(separator: "\n"))
        label.font = .systemFont(ofSize: NSFont.smallSystemFontSize)
        label.textColor = .secondaryLabelColor
        label.preferredMaxLayoutWidth = 380
        stack.addArrangedSubview(label)
      }
      checkboxes.append((checkbox, (group["permissions"] as? [String]) ?? []))
    }
    let origin = NSTextField(labelWithString: (frame["origin"] as? String) ?? "")
    origin.font = .systemFont(ofSize: NSFont.smallSystemFontSize)
    origin.textColor = .tertiaryLabelColor
    stack.addArrangedSubview(origin)
    stack.translatesAutoresizingMaskIntoConstraints = false
    let size = stack.fittingSize
    let scroll = NSScrollView(frame: NSRect(x: 0, y: 0, width: 400, height: min(max(size.height, 40), 360)))
    scroll.hasVerticalScroller = size.height > 360
    scroll.drawsBackground = false
    let document = NSView(frame: NSRect(x: 0, y: 0, width: 384, height: size.height))
    document.addSubview(stack)
    NSLayoutConstraint.activate([
      stack.leadingAnchor.constraint(equalTo: document.leadingAnchor),
      stack.trailingAnchor.constraint(lessThanOrEqualTo: document.trailingAnchor),
      stack.topAnchor.constraint(equalTo: document.topAnchor),
    ])
    scroll.documentView = document
    alert.accessoryView = scroll

    // "Don't Allow" is first, so Return chooses it.
    alert.addButton(withTitle: kind == "review" ? "Cancel" : "Don't Allow")
    let allow = alert.addButton(withTitle: kind == "review" ? "Save" : "Allow")
    allow.isEnabled = false
    DispatchQueue.main.asyncAfter(deadline: .now() + 1.0) { allow.isEnabled = true }

    window.makeKeyAndOrderFront(nil)
    NSApp.activate(ignoringOtherApps: true)
    if let directory = ProcessInfo.processInfo.environment["DYNAPP_HOST_SNAPSHOT"] {
      DispatchQueue.main.asyncAfter(deadline: .now() + 1.5) {
        snapshot(alert.window, to: directory + "/permission-sheet.png")
        if let main = self.window { snapshot(main, to: directory + "/window.png") }
      }
    }
    alert.beginSheetModal(for: window) { [weak self] response in
      if response == .alertSecondButtonReturn {
        var capabilities: [String] = []
        for (checkbox, permissions) in checkboxes where checkbox.state == .on {
          capabilities.append(contentsOf: permissions)
        }
        reply(capabilities)
      } else {
        reply(nil)
      }
      self?.permissionSheetOpen = false
      self?.showNextPermissionRequest()
    }
  }

  func showError(_ text: String) {
    guard let window else { return }
    let alert = NSAlert()
    alert.messageText = settings.appName
    alert.informativeText = text
    alert.beginSheetModal(for: window, completionHandler: nil)
  }

  // MARK: Navigation

  func isAppOrigin(_ url: URL?) -> Bool {
    originKey(url) == originKey(settings.origin)
  }

  /// Popups for Dyner sign-in stay in the app; other sites open in the
  /// default browser.
  func isDynerHost(_ url: URL?) -> Bool {
    guard let host = url?.host?.lowercased(), let appHost = settings.origin.host?.lowercased() else { return false }
    let parts = appHost.split(separator: ".")
    guard parts.count >= 2 else { return false }
    let base = parts.suffix(2).joined(separator: ".")
    return host == base || host.hasSuffix("." + base)
  }

  func webView(_ webView: WKWebView, decidePolicyFor navigationAction: WKNavigationAction,
               decisionHandler: @escaping (WKNavigationActionPolicy) -> Void) {
    if navigationAction.shouldPerformDownload {
      decisionHandler(.download)
      return
    }
    guard let url = navigationAction.request.url else {
      decisionHandler(.cancel)
      return
    }
    let scheme = url.scheme?.lowercased() ?? ""
    let mainFrame = navigationAction.targetFrame?.isMainFrame ?? true
    if webView !== self.webView || !mainFrame || ["about", "blob", "data"].contains(scheme) || isAppOrigin(url) {
      decisionHandler(.allow)
      return
    }
    if ["http", "https", "mailto", "tel"].contains(scheme) {
      NSWorkspace.shared.open(url)
    }
    decisionHandler(.cancel)
  }

  func webView(_ webView: WKWebView, decidePolicyFor navigationResponse: WKNavigationResponse,
               decisionHandler: @escaping (WKNavigationResponsePolicy) -> Void) {
    decisionHandler(navigationResponse.canShowMIMEType ? .allow : .download)
  }

  func webView(_ webView: WKWebView, createWebViewWith configuration: WKWebViewConfiguration,
               for navigationAction: WKNavigationAction, windowFeatures: WKWindowFeatures) -> WKWebView? {
    let url = navigationAction.request.url
    let scheme = url?.scheme?.lowercased() ?? "about"
    if url != nil && scheme != "about" && !isAppOrigin(url) && !isDynerHost(url) {
      if let url { NSWorkspace.shared.open(url) }
      return nil
    }
    let popup = WKWebView(frame: NSRect(x: 0, y: 0, width: 520, height: 680), configuration: configuration)
    popup.uiDelegate = self
    popup.navigationDelegate = self
    let popupWindow = NSWindow(
      contentRect: popup.frame, styleMask: [.titled, .closable, .resizable], backing: .buffered, defer: false)
    popupWindow.title = settings.appName
    popupWindow.contentView = popup
    popupWindow.isReleasedWhenClosed = false
    popupWindow.center()
    popupWindow.makeKeyAndOrderFront(nil)
    popups.append(popupWindow)
    return popup
  }

  func webViewDidClose(_ webView: WKWebView) {
    if let index = popups.firstIndex(where: { $0.contentView === webView }) {
      popups[index].close()
      popups.remove(at: index)
    }
  }

  func webView(_ webView: WKWebView, navigationAction: WKNavigationAction, didBecome download: WKDownload) {
    download.delegate = self
  }

  func webView(_ webView: WKWebView, navigationResponse: WKNavigationResponse, didBecome download: WKDownload) {
    download.delegate = self
  }

  func download(_ download: WKDownload, decideDestinationUsing response: URLResponse, suggestedFilename: String,
                completionHandler: @escaping (URL?) -> Void) {
    let folder = FileManager.default.urls(for: .downloadsDirectory, in: .userDomainMask).first
      ?? URL(fileURLWithPath: NSHomeDirectory())
    let name = suggestedFilename.isEmpty ? "download" : suggestedFilename
    var candidate = folder.appendingPathComponent(name)
    let base = candidate.deletingPathExtension().lastPathComponent
    let ext = candidate.pathExtension
    var counter = 2
    while FileManager.default.fileExists(atPath: candidate.path) {
      candidate = folder.appendingPathComponent(ext.isEmpty ? "\(base) \(counter)" : "\(base) \(counter).\(ext)")
      counter += 1
    }
    completionHandler(candidate)
  }

  func downloadDidFinish(_ download: WKDownload) {}

  func download(_ download: WKDownload, didFailWithError error: Error, resumeData: Data?) {
    showError("The download failed: \(error.localizedDescription)")
  }

  // MARK: Page UI

  func webView(_ webView: WKWebView, runOpenPanelWith parameters: WKOpenPanelParameters,
               initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping ([URL]?) -> Void) {
    let panel = NSOpenPanel()
    panel.allowsMultipleSelection = parameters.allowsMultipleSelection
    panel.canChooseDirectories = parameters.allowsDirectories
    panel.canChooseFiles = true
    panel.beginSheetModal(for: window) { response in
      completionHandler(response == .OK ? panel.urls : nil)
    }
  }

  func webView(_ webView: WKWebView, runJavaScriptAlertPanelWithMessage message: String,
               initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping () -> Void) {
    let alert = NSAlert()
    alert.messageText = message
    alert.beginSheetModal(for: window) { _ in completionHandler() }
  }

  func webView(_ webView: WKWebView, runJavaScriptConfirmPanelWithMessage message: String,
               initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping (Bool) -> Void) {
    let alert = NSAlert()
    alert.messageText = message
    alert.addButton(withTitle: "OK")
    alert.addButton(withTitle: "Cancel")
    alert.beginSheetModal(for: window) { completionHandler($0 == .alertFirstButtonReturn) }
  }

  func webView(_ webView: WKWebView, runJavaScriptTextInputPanelWithPrompt prompt: String, defaultText: String?,
               initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping (String?) -> Void) {
    let alert = NSAlert()
    alert.messageText = prompt
    let field = NSTextField(frame: NSRect(x: 0, y: 0, width: 300, height: 24))
    field.stringValue = defaultText ?? ""
    alert.accessoryView = field
    alert.addButton(withTitle: "OK")
    alert.addButton(withTitle: "Cancel")
    alert.window.initialFirstResponder = field
    alert.beginSheetModal(for: window) { completionHandler($0 == .alertFirstButtonReturn ? field.stringValue : nil) }
  }

  func webView(_ webView: WKWebView, requestMediaCapturePermissionFor origin: WKSecurityOrigin,
               initiatedByFrame frame: WKFrameInfo, type: WKMediaCaptureType,
               decisionHandler: @escaping (WKPermissionDecision) -> Void) {
    decisionHandler(originKey(origin) == originKey(settings.origin) ? .prompt : .deny)
  }

  // MARK: Menu

  func buildMenu() {
    let main = NSMenu()
    let appItem = NSMenuItem()
    let appMenu = NSMenu()
    appMenu.addItem(withTitle: "About \(settings.appName)", action: #selector(NSApplication.orderFrontStandardAboutPanel(_:)), keyEquivalent: "")
    appMenu.addItem(.separator())
    appMenu.addItem(withTitle: "Permissions…", action: #selector(reviewPermissions(_:)), keyEquivalent: ",").target = self
    appMenu.addItem(.separator())
    appMenu.addItem(withTitle: "Hide \(settings.appName)", action: #selector(NSApplication.hide(_:)), keyEquivalent: "h")
    let hideOthers = appMenu.addItem(withTitle: "Hide Others", action: #selector(NSApplication.hideOtherApplications(_:)), keyEquivalent: "h")
    hideOthers.keyEquivalentModifierMask = [.command, .option]
    appMenu.addItem(withTitle: "Show All", action: #selector(NSApplication.unhideAllApplications(_:)), keyEquivalent: "")
    appMenu.addItem(.separator())
    appMenu.addItem(withTitle: "Quit \(settings.appName)", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
    appItem.submenu = appMenu
    main.addItem(appItem)

    let editItem = NSMenuItem()
    let edit = NSMenu(title: "Edit")
    edit.addItem(withTitle: "Undo", action: Selector(("undo:")), keyEquivalent: "z")
    let redo = edit.addItem(withTitle: "Redo", action: Selector(("redo:")), keyEquivalent: "z")
    redo.keyEquivalentModifierMask = [.command, .shift]
    edit.addItem(.separator())
    edit.addItem(withTitle: "Cut", action: #selector(NSText.cut(_:)), keyEquivalent: "x")
    edit.addItem(withTitle: "Copy", action: #selector(NSText.copy(_:)), keyEquivalent: "c")
    edit.addItem(withTitle: "Paste", action: #selector(NSText.paste(_:)), keyEquivalent: "v")
    edit.addItem(withTitle: "Select All", action: #selector(NSText.selectAll(_:)), keyEquivalent: "a")
    editItem.submenu = edit
    main.addItem(editItem)

    let viewItem = NSMenuItem()
    let view = NSMenu(title: "View")
    view.addItem(withTitle: "Reload", action: #selector(reloadPage(_:)), keyEquivalent: "r").target = self
    let fullScreen = view.addItem(withTitle: "Enter Full Screen", action: #selector(NSWindow.toggleFullScreen(_:)), keyEquivalent: "f")
    fullScreen.keyEquivalentModifierMask = [.command, .control]
    viewItem.submenu = view
    main.addItem(viewItem)

    let windowItem = NSMenuItem()
    let windowMenu = NSMenu(title: "Window")
    windowMenu.addItem(withTitle: "Minimize", action: #selector(NSWindow.performMiniaturize(_:)), keyEquivalent: "m")
    windowMenu.addItem(withTitle: "Zoom", action: #selector(NSWindow.performZoom(_:)), keyEquivalent: "")
    windowMenu.addItem(withTitle: "Close", action: #selector(NSWindow.performClose(_:)), keyEquivalent: "w")
    windowItem.submenu = windowMenu
    main.addItem(windowItem)
    NSApp.windowsMenu = windowMenu
    NSApp.mainMenu = main
  }

  @objc func reloadPage(_ sender: Any?) {
    webView?.reload()
  }
}

/// Renders a window without Screen Recording access. Used only when the host
/// is launched with DYNAPP_HOST_SNAPSHOT for UI checks.
func snapshot(_ window: NSWindow, to path: String) {
  guard let view = window.contentView?.superview ?? window.contentView,
    let representation = view.bitmapImageRepForCachingDisplay(in: view.bounds)
  else { return }
  view.cacheDisplay(in: view.bounds, to: representation)
  try? representation.representation(using: .png, properties: [:])?.write(to: URL(fileURLWithPath: path))
}

guard let settings = HostSettings.load() else {
  let alert = NSAlert()
  alert.messageText = "This DynApp app is damaged"
  alert.informativeText = "Install it again from Dyner."
  alert.runModal()
  exit(1)
}

let application = NSApplication.shared
let controller = HostController(settings: settings)
application.delegate = controller
application.setActivationPolicy(.regular)
application.run()
