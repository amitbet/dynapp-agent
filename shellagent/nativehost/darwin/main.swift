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
  var overlayTitlebar = false

  override func mouseDown(with event: NSEvent) {
    let point = convert(event.locationInWindow, from: nil)
    let top = isFlipped ? point.y : bounds.height - point.y
    // Controls occupy the trailing area. Dragging and double-clicking the
    // leading title area use the original NSEvent, preserving native behavior.
    if overlayTitlebar && !(window?.styleMask.contains(.fullScreen) ?? false)
      && top < 36 && point.x > 84 && point.x < bounds.width - 360 {
      if event.clickCount == 2 { window?.performZoom(nil) }
      else { window?.performDrag(with: event) }
      return
    }
    super.mouseDown(with: event)
  }

  override func performDragOperation(_ sender: NSDraggingInfo) -> Bool {
    let options: [NSPasteboard.ReadingOptionKey: Any] = [.urlReadingFileURLsOnly: true]
    let urls = sender.draggingPasteboard.readObjects(forClasses: [NSURL.self], options: options) as? [URL] ?? []
    let local = convert(sender.draggingLocation, from: nil)
    let point = CGPoint(x: local.x, y: isFlipped ? local.y : bounds.height - local.y)
    let handled = super.performDragOperation(sender)
    if !urls.isEmpty { onFileDrop?(urls.map(\.path), point) }
    return handled
  }

  /// Automation clicks go straight to WebKit, skipping the overlay title bar
  /// drag area, so the page receives trusted mouse events.
  func automationClick(down: NSEvent, up: NSEvent, moved: NSEvent?) {
    if let moved { super.mouseMoved(with: moved) }
    super.mouseDown(with: down)
    super.mouseUp(with: up)
  }
}

// MARK: - Host controller

final class HostController: NSObject, NSApplicationDelegate, WKScriptMessageHandler, WKNavigationDelegate,
  WKUIDelegate, WKDownloadDelegate, NSWindowDelegate
{
  let settings: HostSettings
  var window: NSWindow!
  var webView: HostWebView!
  var pendingOpenFiles: [[String]] = []
  var control: AgentConnection?
  var controlReconnectPending = false
  var agentOrigin = ""
  var sockets: [String: PageSocket] = [:]
  var popups: [NSWindow] = []
  var pendingReviews: [String: String] = [:]
  var permissionQueue: [(frame: [String: Any], reply: ([String]?) -> Void)] = []
  var shownRequests = Set<String>()
  var permissionSheetOpen = false
  var bootstrapScript: String?
  var automationEnabled = false
  var pendingScripts: [String: (Any?, String?) -> Void] = [:]
  var stopAutomationItem: NSMenuItem?

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

  func application(_ sender: NSApplication, openFiles filenames: [String]) {
    pendingOpenFiles.append(filenames)
    window?.makeKeyAndOrderFront(nil)
    NSApp.activate(ignoringOtherApps: true)
    flushOpenFiles()
  }

  func flushOpenFiles() {
    guard let control else { return }
    for paths in pendingOpenFiles {
      control.sendJSON(["type": "native-host-open-files", "paths": paths])
    }
    pendingOpenFiles.removeAll()
  }

  /// The control connection supplies the bridge script and start URL. Without
  /// an agent the app still loads as a plain web app.
  ///
  /// A reconnect restores the channel after the agent restarts (for example on
  /// an update) without reloading the page: its sockets reconnect on their own,
  /// but Finder opens and reviews only travel over this channel.
  func connectControl(attempt: Int, reconnect: Bool = false) {
    guard let connection = AgentConnection(path: settings.socketPath) else {
      if reconnect {
        scheduleControlReconnect(attempt: attempt + 1)
        return
      }
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
      if reconnect {
        self.scheduleControlReconnect(attempt: attempt + 1)
      } else {
        self.loadPage(bootstrap: nil, url: self.settings.startURL)
      }
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
          self.flushOpenFiles()
          let automation = (frame["automation"] as? Bool) ?? false
          if reconnect {
            if automation != self.automationEnabled { self.applyAutomation(automation) }
            return
          }
          self.automationEnabled = automation
          self.window.subtitle = automation ? "Automation on" : ""
          self.stopAutomationItem?.isHidden = !automation
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
        case "native-host-open-files-result":
          if let error = frame["error"] as? String {
            self.showError(error)
            NSApp.reply(toOpenOrPrint: .failure)
          } else {
            NSApp.reply(toOpenOrPrint: .success)
          }
        case "native-host-review-result":
          self.finishReview(frame)
        case "native-host-automation":
          self.handleAutomation(frame, connection: connection)
        case "native-host-automation-state":
          self.applyAutomation((frame["enabled"] as? Bool) ?? false)
        case "native-host-error":
          guard !loaded else { return }
          loaded = true
          timeout.cancel()
          self.showError(frame["error"] as? String ?? "The DynApp agent refused this app.")
          if reconnect { return }
          self.loadPage(bootstrap: nil, url: self.settings.startURL)
        default:
          break
        }
      }
    }
    connection.onClose = { [weak self] in
      DispatchQueue.main.async {
        guard let self else { return }
        if self.control === connection {
          self.control = nil
          self.scheduleControlReconnect(attempt: 0)
        }
        for (rid, _) in self.pendingReviews {
          self.deliver(["rid": rid, "event": "review", "error": "The DynApp agent is not running"])
        }
        self.pendingReviews.removeAll()
      }
    }
    connection.start()
    connection.sendJSON([
      "type": "native-host-hello", "version": 1, "storeId": settings.storeID, "purpose": "control",
      "features": ["automation"],
    ])
  }

  func scheduleControlReconnect(attempt: Int) {
    guard control == nil, !controlReconnectPending else { return }
    controlReconnectPending = true
    let delay = min(0.5 * pow(2, Double(min(attempt, 4))), 5)
    DispatchQueue.main.asyncAfter(deadline: .now() + delay) {
      self.controlReconnectPending = false
      guard self.control == nil else { return }
      self.connectControl(attempt: attempt, reconnect: true)
    }
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
      bootstrapScript = bootstrap
      content.add(self, contentWorld: .page, name: "dynappNativeHost")
    }
    configuration.userContentController = content
    installUserScripts(content)
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
    case "titlebar":
      let enabled = body["enabled"] as? Bool ?? false
      webView.overlayTitlebar = enabled
      if enabled { window.styleMask.insert(.fullSizeContentView) }
      else { window.styleMask.remove(.fullSizeContentView) }
      window.titlebarAppearsTransparent = enabled
      window.titleVisibility = enabled ? .hidden : .visible
    case "titlebarDrag":
      if webView.overlayTitlebar, let event = NSApp.currentEvent, event.type == .leftMouseDown {
        window.performDrag(with: event)
      }
    case "titlebarZoom":
      if webView.overlayTitlebar { window.performZoom(nil) }
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
    case "console":
      guard automationEnabled, let entries = body["entries"] as? [Any] else { return }
      control?.sendJSON(["type": "native-host-console", "entries": entries])
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

  // MARK: Automation

  /// The bridge and its flags are injected at document start of every load.
  /// The automation flag is a separate script, so turning automation on
  /// takes effect from the next page load without a new bridge.
  func installUserScripts(_ content: WKUserContentController) {
    content.removeAllUserScripts()
    guard let bootstrap = bootstrapScript, !bootstrap.isEmpty else { return }
    content.addUserScript(WKUserScript(source: "Object.defineProperty(window, '__DYNAPP_NATIVE_TITLEBAR__', {value: true});", injectionTime: .atDocumentStart, forMainFrameOnly: true, in: .page))
    if automationEnabled {
      content.addUserScript(WKUserScript(source: "Object.defineProperty(window, '__DYNAPP_NATIVE_AUTOMATION__', {value: true});", injectionTime: .atDocumentStart, forMainFrameOnly: true, in: .page))
    }
    content.addUserScript(WKUserScript(source: bootstrap, injectionTime: .atDocumentStart, forMainFrameOnly: true, in: .page))
  }

  func applyAutomation(_ enabled: Bool) {
    automationEnabled = enabled
    window?.subtitle = enabled ? "Automation on" : ""
    stopAutomationItem?.isHidden = !enabled
    if let content = webView?.configuration.userContentController { installUserScripts(content) }
    deliver(["event": "automation", "enabled": enabled])
  }

  @objc func stopAutomation(_ sender: Any?) {
    control?.sendJSON(["type": "native-host-automation-stop"])
    applyAutomation(false)
  }

  func webView(_ webView: WKWebView, didCommit navigation: WKNavigation!) {
    guard webView === self.webView, !pendingScripts.isEmpty else { return }
    let pending = pendingScripts
    pendingScripts.removeAll()
    for (_, reply) in pending { reply(nil, "The page navigated before the script finished (no longer reachable)") }
  }

  func webView(_ webView: WKWebView, didFinish navigation: WKNavigation!) {
    guard webView === self.webView, automationEnabled else { return }
    deliver(["event": "automation", "enabled": true])
    control?.sendJSON(["type": "native-host-console", "entries": [[
      "level": "info", "source": "navigation", "text": "Loaded \(webView.url?.absoluteString ?? "")",
      "time": Int64(Date().timeIntervalSince1970 * 1000),
    ]]])
  }

  /// Runs one command from the agent's automation socket. The agent checks
  /// that the user allowed automation before it sends anything but "consent".
  func handleAutomation(_ frame: [String: Any], connection: AgentConnection) {
    guard let id = frame["id"] as? String, let op = frame["op"] as? String else { return }
    let reply: (Any?, String?) -> Void = { value, error in
      var result: [String: Any] = ["type": "native-host-automation-result", "id": id]
      if let error { result["error"] = error } else { result["value"] = value ?? NSNull() }
      if !connection.sendJSON(result) {
        connection.sendJSON(["type": "native-host-automation-result", "id": id, "error": "The result could not be encoded"])
      }
    }
    if op == "consent" { return presentAutomationConsent(reply) }
    guard automationEnabled else { return reply(nil, "Automation is off for this app") }
    guard let webView, let window else { return reply(nil, "The app has not loaded a page yet") }
    switch op {
    case "eval":
      guard let script = frame["script"] as? String else { return reply(nil, "No script") }
      // A page load can leave the completion uncalled; didCommit fails it.
      pendingScripts[id] = reply
      runAutomationScript(script, fallback: frame["fallback"] as? String, in: webView) { [weak self] value, error in
        guard let self, let pending = self.pendingScripts.removeValue(forKey: id) else { return }
        pending(value, error)
      }
    case "snapshot":
      if (frame["window"] as? Bool) == true {
        guard let view = window.contentView?.superview ?? window.contentView,
          let representation = view.bitmapImageRepForCachingDisplay(in: view.bounds)
        else { return reply(nil, "The window snapshot failed") }
        view.cacheDisplay(in: view.bounds, to: representation)
        guard let png = representation.representation(using: .png, properties: [:]) else { return reply(nil, "The window snapshot failed") }
        return reply(["png": png.base64EncodedString(), "width": representation.pixelsWide, "height": representation.pixelsHigh], nil)
      }
      let configuration = WKSnapshotConfiguration()
      configuration.afterScreenUpdates = true
      // By default one image pixel is one CSS pixel, so coordinates read off
      // the image can be passed to click as they are.
      let width = max(1, min((frame["width"] as? Double) ?? Double(webView.bounds.width), 8192))
      webView.takeSnapshot(with: configuration) { image, error in
        guard let image, image.size.width > 0 else { return reply(nil, error?.localizedDescription ?? "The snapshot failed") }
        let height = width * Double(image.size.height / image.size.width)
        guard let bitmap = NSBitmapImageRep(
          bitmapDataPlanes: nil, pixelsWide: Int(width.rounded()), pixelsHigh: Int(height.rounded()), bitsPerSample: 8,
          samplesPerPixel: 4, hasAlpha: true, isPlanar: false, colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0)
        else { return reply(nil, "The snapshot failed") }
        bitmap.size = NSSize(width: width, height: height)
        NSGraphicsContext.saveGraphicsState()
        NSGraphicsContext.current = NSGraphicsContext(bitmapImageRep: bitmap)
        NSGraphicsContext.current?.imageInterpolation = .high
        image.draw(in: NSRect(x: 0, y: 0, width: width, height: height))
        NSGraphicsContext.restoreGraphicsState()
        guard let png = bitmap.representation(using: .png, properties: [:]) else { return reply(nil, "The snapshot failed") }
        reply([
          "png": png.base64EncodedString(), "width": bitmap.pixelsWide, "height": bitmap.pixelsHigh,
          "viewportWidth": Double(webView.bounds.width), "viewportHeight": Double(webView.bounds.height),
        ], nil)
      }
    case "reload":
      webView.reload()
      reply(["reloaded": true], nil)
    case "navigate":
      guard let text = frame["url"] as? String, let url = URL(string: text), isAppOrigin(url) else {
        return reply(nil, "Automation can only navigate inside the app")
      }
      webView.load(URLRequest(url: url))
      reply(["url": url.absoluteString], nil)
    case "click":
      let x = (frame["x"] as? Double) ?? 0, y = (frame["y"] as? Double) ?? 0
      guard x >= 0, y >= 0, x <= Double(webView.bounds.width), y <= Double(webView.bounds.height) else {
        return reply(nil, "The point is outside the app's viewport")
      }
      let local = NSPoint(x: x, y: webView.isFlipped ? y : Double(webView.bounds.height) - y)
      let location = webView.convert(local, to: nil)
      window.makeFirstResponder(webView)
      let count = max(1, min((frame["count"] as? Int) ?? 1, 3))
      var steps: [() -> Void] = []
      for click in 1...count {
        steps.append {
          let time = ProcessInfo.processInfo.systemUptime
          guard
            let down = NSEvent.mouseEvent(with: .leftMouseDown, location: location, modifierFlags: [], timestamp: time,
              windowNumber: window.windowNumber, context: nil, eventNumber: 0, clickCount: click, pressure: 1),
            let up = NSEvent.mouseEvent(with: .leftMouseUp, location: location, modifierFlags: [], timestamp: time,
              windowNumber: window.windowNumber, context: nil, eventNumber: 0, clickCount: click, pressure: 0)
          else { return }
          let moved = click == 1 ? NSEvent.mouseEvent(with: .mouseMoved, location: location, modifierFlags: [], timestamp: time,
            windowNumber: window.windowNumber, context: nil, eventNumber: 0, clickCount: 0, pressure: 0) : nil
          webView.automationClick(down: down, up: up, moved: moved)
        }
      }
      runInputSteps(steps, in: webView) { reply(["x": x, "y": y], nil) }
    case "type":
      guard let text = frame["text"] as? String else { return reply(nil, "No text") }
      window.makeFirstResponder(webView)
      let steps: [() -> Void] = text.map { character in
        let value = String(character)
        return { [weak self] in
          guard let self else { return }
          switch value {
          case "\n", "\r": self.sendKey(code: 36, characters: "\r", plain: "\r", flags: [], to: webView)
          case "\t": self.sendKey(code: 48, characters: "\t", plain: "\t", flags: [], to: webView)
          default:
            let lower = value.lowercased()
            let code = HostController.keyCodes[lower] ?? 0
            self.sendKey(code: code, characters: value, plain: lower, flags: value != lower ? [.shift] : [], to: webView)
          }
        }
      }
      runInputSteps(steps, in: webView) { reply(["typed": text.count], nil) }
    case "key":
      guard let name = frame["key"] as? String, let key = HostController.namedKey(name) else {
        return reply(nil, "Unknown key; use a character or Enter, Tab, Escape, Backspace, Delete, Space, Arrow*, Home, End, PageUp, PageDown, F1-F12")
      }
      var flags: NSEvent.ModifierFlags = []
      for modifier in (frame["modifiers"] as? [String]) ?? [] {
        switch modifier {
        case "meta": flags.insert(.command)
        case "control": flags.insert(.control)
        case "alt": flags.insert(.option)
        case "shift": flags.insert(.shift)
        default: break
        }
      }
      window.makeFirstResponder(webView)
      let characters = flags.contains(.shift) ? key.characters.uppercased() : key.characters
      runInputSteps([{ [weak self] in
        self?.sendKey(code: key.code, characters: characters, plain: key.characters, flags: flags, to: webView)
      }], in: webView) { reply(["key": name], nil) }
    case "window":
      if frame["width"] != nil || frame["height"] != nil {
        let current = webView.bounds.size
        let width = (frame["width"] as? Double) ?? Double(current.width)
        let height = (frame["height"] as? Double) ?? Double(current.height)
        window.setContentSize(NSSize(width: max(width, window.minSize.width), height: max(height, window.minSize.height)))
      }
      if frame["x"] != nil || frame["y"] != nil, let screen = window.screen ?? NSScreen.main {
        let x = (frame["x"] as? Double) ?? Double(window.frame.minX)
        let y = (frame["y"] as? Double) ?? Double(screen.frame.maxY - window.frame.maxY)
        window.setFrameTopLeftPoint(NSPoint(x: x, y: Double(screen.frame.maxY) - y))
      }
      if (frame["focus"] as? Bool) == true {
        window.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
      }
      let screenTop = Double((window.screen ?? NSScreen.main)?.frame.maxY ?? window.frame.maxY)
      reply([
        "x": Double(window.frame.minX), "y": screenTop - Double(window.frame.maxY),
        "width": Double(window.frame.width), "height": Double(window.frame.height),
        "viewportWidth": Double(webView.bounds.width), "viewportHeight": Double(webView.bounds.height),
        "scale": Double(window.backingScaleFactor), "key": window.isKeyWindow, "visible": window.isVisible,
        "minimized": window.isMiniaturized, "fullScreen": window.styleMask.contains(.fullScreen),
        "url": webView.url?.absoluteString ?? "", "title": webView.title ?? "",
      ], nil)
    default:
      reply(nil, "This app host does not support \(op)")
    }
  }

  /// WebKit drops input that arrives while the web process is still handling
  /// earlier input. A script round trip after each step is a barrier: input
  /// and scripts travel to the web process in order.
  func runInputSteps(_ steps: [() -> Void], in webView: WKWebView, done: @escaping () -> Void) {
    guard let step = steps.first else { return done() }
    step()
    webView.callAsyncJavaScript("return 0", arguments: [:], in: nil, in: .page) { [weak self] _ in
      DispatchQueue.main.asyncAfter(deadline: .now() + 0.01) {
        self?.runInputSteps(Array(steps.dropFirst()), in: webView, done: done)
      }
    }
  }

  func runAutomationScript(_ script: String, fallback: String?, in webView: WKWebView, reply: @escaping (Any?, String?) -> Void) {
    webView.callAsyncJavaScript(script, arguments: [:], in: nil, in: .page) { [weak self] result in
      switch result {
      case .success(let value):
        reply((value as? String) ?? "", nil)
      case .failure(let error):
        let info = (error as NSError).userInfo
        let message = (info["WKJavaScriptExceptionMessage"] as? String) ?? error.localizedDescription
        if let fallback, message.contains("SyntaxError") || message.contains("Unexpected") {
          self?.runAutomationScript(fallback, fallback: nil, in: webView, reply: reply)
        } else {
          reply(nil, message)
        }
      }
    }
  }

  func sendKey(code: UInt16, characters: String, plain: String, flags: NSEvent.ModifierFlags, to webView: WKWebView) {
    let time = ProcessInfo.processInfo.systemUptime
    guard let windowNumber = webView.window?.windowNumber,
      let down = NSEvent.keyEvent(with: .keyDown, location: .zero, modifierFlags: flags, timestamp: time, windowNumber: windowNumber,
        context: nil, characters: characters, charactersIgnoringModifiers: plain, isARepeat: false, keyCode: code),
      let up = NSEvent.keyEvent(with: .keyUp, location: .zero, modifierFlags: flags, timestamp: time, windowNumber: windowNumber,
        context: nil, characters: characters, charactersIgnoringModifiers: plain, isARepeat: false, keyCode: code)
    else { return }
    // Shortcuts go to the page, never to this app's menu (Cmd+Q, Cmd+W).
    if !flags.contains(.command) || !webView.performKeyEquivalent(with: down) {
      webView.keyDown(with: down)
    }
    webView.keyUp(with: up)
  }

  static let keyCodes: [String: UInt16] = [
    "a": 0, "s": 1, "d": 2, "f": 3, "h": 4, "g": 5, "z": 6, "x": 7, "c": 8, "v": 9, "b": 11, "q": 12, "w": 13, "e": 14,
    "r": 15, "y": 16, "t": 17, "1": 18, "2": 19, "3": 20, "4": 21, "6": 22, "5": 23, "=": 24, "9": 25, "7": 26, "-": 27,
    "8": 28, "0": 29, "]": 30, "o": 31, "u": 32, "[": 33, "i": 34, "p": 35, "l": 37, "j": 38, "'": 39, "k": 40, ";": 41,
    "\\": 42, ",": 43, "/": 44, "n": 45, "m": 46, ".": 47, "`": 50, " ": 49,
  ]

  static func namedKey(_ name: String) -> (code: UInt16, characters: String)? {
    func function(_ value: Int) -> String { String(UnicodeScalar(UInt32(value)).map(Character.init) ?? " ") }
    switch name.lowercased() {
    case "enter", "return": return (36, "\r")
    case "tab": return (48, "\t")
    case "escape", "esc": return (53, "\u{1b}")
    case "backspace": return (51, "\u{7f}")
    case "delete", "del": return (117, function(NSDeleteFunctionKey))
    case "space": return (49, " ")
    case "arrowleft", "left": return (123, function(NSLeftArrowFunctionKey))
    case "arrowright", "right": return (124, function(NSRightArrowFunctionKey))
    case "arrowdown", "down": return (125, function(NSDownArrowFunctionKey))
    case "arrowup", "up": return (126, function(NSUpArrowFunctionKey))
    case "home": return (115, function(NSHomeFunctionKey))
    case "end": return (119, function(NSEndFunctionKey))
    case "pageup": return (116, function(NSPageUpFunctionKey))
    case "pagedown": return (121, function(NSPageDownFunctionKey))
    default: break
    }
    let functionKeys: [UInt16] = [122, 120, 99, 118, 96, 97, 98, 100, 101, 109, 103, 111]
    if name.count >= 2, name.first == "F" || name.first == "f", let number = Int(name.dropFirst()), (1...12).contains(number) {
      return (functionKeys[number - 1], function(NSF1FunctionKey + number - 1))
    }
    if name.count == 1 {
      let lower = name.lowercased()
      return (keyCodes[lower] ?? 0, lower)
    }
    return nil
  }

  /// Asks the user in this app's window. Only the user can turn automation on.
  func presentAutomationConsent(_ reply: @escaping (Any?, String?) -> Void) {
    guard let window else { return reply(nil, "The app has no window yet") }
    if permissionSheetOpen || window.attachedSheet != nil {
      return reply(nil, "Another dialog is open in the app; answer it first")
    }
    permissionSheetOpen = true
    let name = settings.appName
    let alert = NSAlert()
    alert.alertStyle = .warning
    alert.messageText = "Allow coding agents to control \(name)?"
    alert.informativeText = "A coding agent on this Mac, such as Codex or Claude Code, asked to control \(name): run scripts in it, read what it shows, take screenshots, and click and type in it. It can use everything \(name) is allowed to do.\n\nAllow this only if you started that agent. You can turn it off from \(name) ▸ Stop Automation."
    alert.addButton(withTitle: "Don't Allow")
    let allow = alert.addButton(withTitle: "Allow")
    allow.isEnabled = false
    DispatchQueue.main.asyncAfter(deadline: .now() + 1.0) { allow.isEnabled = true }
    window.makeKeyAndOrderFront(nil)
    NSApp.activate(ignoringOtherApps: true)
    if let directory = ProcessInfo.processInfo.environment["DYNAPP_HOST_SNAPSHOT"] {
      DispatchQueue.main.asyncAfter(deadline: .now() + 1.5) {
        snapshot(alert.window, to: directory + "/automation-consent.png")
      }
    }
    alert.beginSheetModal(for: window) { [weak self] response in
      reply(["approved": response == .alertSecondButtonReturn], nil)
      self?.permissionSheetOpen = false
      self?.showNextPermissionRequest()
    }
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

  // Page saves (blob/data downloads) are explicit user actions such as "Save as",
  // so ask where to put them like a browser with "Ask where to save" enabled.
  func download(_ download: WKDownload, decideDestinationUsing response: URLResponse, suggestedFilename: String,
                completionHandler: @escaping (URL?) -> Void) {
    let panel = NSSavePanel()
    panel.directoryURL = FileManager.default.urls(for: .downloadsDirectory, in: .userDomainMask).first
    panel.nameFieldStringValue = suggestedFilename.isEmpty ? "download" : suggestedFilename
    panel.canCreateDirectories = true
    panel.isExtensionHidden = false
    let finish: (NSApplication.ModalResponse) -> Void = { response in
      guard response == .OK, let url = panel.url else { return completionHandler(nil) }
      // NSSavePanel already confirmed the overwrite; WKDownload refuses existing files.
      if FileManager.default.fileExists(atPath: url.path) { try? FileManager.default.removeItem(at: url) }
      completionHandler(url)
    }
    if let window, window.isVisible { panel.beginSheetModal(for: window, completionHandler: finish) }
    else { finish(panel.runModal()) }
  }

  func downloadDidFinish(_ download: WKDownload) {}

  func download(_ download: WKDownload, didFailWithError error: Error, resumeData: Data?) {
    showError("The download failed: \(error.localizedDescription)")
  }

  // MARK: Printing

  // WebKit has no public WKUIDelegate hook for window.print(); this private
  // selector is what Safari-style hosts implement. Without it the call is a no-op.
  @objc(_webView:printFrame:)
  func webViewPrintFrame(_ webView: WKWebView, printFrame frame: AnyObject?) {
    // Run after WebKit returns from the request so the page can lay out for print.
    DispatchQueue.main.async { [weak self] in self?.printPage(webView) }
  }

  func printPage(_ webView: WKWebView) {
    let info = (NSPrintInfo.shared.copy() as? NSPrintInfo) ?? NSPrintInfo()
    info.horizontalPagination = .automatic
    info.verticalPagination = .automatic
    info.isHorizontallyCentered = false
    info.isVerticallyCentered = false
    let operation = webView.printOperation(with: info)
    operation.showsPrintPanel = true
    operation.showsProgressPanel = true
    operation.printPanel.options.formUnion([.showsOrientation, .showsPaperSize, .showsScaling, .showsPreview])
    // printOperation's view has a zero frame and prints blank pages otherwise.
    operation.view?.frame = webView.bounds
    let host = webView.window ?? window
    if let host { operation.runModal(for: host, delegate: nil, didRun: nil, contextInfo: nil) }
    else { operation.run() }
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
    let stop = appMenu.addItem(withTitle: "Stop Automation", action: #selector(stopAutomation(_:)), keyEquivalent: "")
    stop.target = self
    stop.isHidden = true
    stopAutomationItem = stop
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
