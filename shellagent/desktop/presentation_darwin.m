//go:build darwin && cgo

#import <AppKit/AppKit.h>
#import <Carbon/Carbon.h>

extern void dynappGoPresentationEvent(char *, char *, unsigned int);
static NSStatusItem *statusItem;
static NSMutableDictionary<NSNumber *, NSValue *> *hotkeys;
static NSPanel *dropPanel;
static NSString *dropTargetID;
static NSTimer *dropLeaseTimer;
static NSTimeInterval dropLeaseDeadline;

static void emitDropEvent(NSString *kind, NSDictionary *event) {
    NSData *data = [NSJSONSerialization dataWithJSONObject:event options:0 error:nil];
    NSString *json = data ? [[NSString alloc] initWithData:data encoding:NSUTF8StringEncoding] : @"{}";
    dynappGoPresentationEvent((char *)kind.UTF8String, (char *)json.UTF8String, 0);
}

static void hideDropPanel(void) {
    [dropPanel orderOut:nil];
    dropTargetID = nil;
    [dropLeaseTimer invalidate];
    dropLeaseTimer = nil;
}

@interface DynappDropView : NSView <NSDraggingDestination>
@end

@implementation DynappDropView
- (NSDragOperation)draggingEntered:(id<NSDraggingInfo>)sender {
    if (!dropTargetID.length) return NSDragOperationNone;
    NSArray *urls = [[sender draggingPasteboard] readObjectsForClasses:@[NSURL.class]
        options:@{NSPasteboardURLReadingFileURLsOnlyKey:@YES}] ?: @[];
    if (!urls.count) return NSDragOperationNone;
    dropLeaseDeadline = NSDate.timeIntervalSinceReferenceDate + 1.0;
    emitDropEvent(@"drop-enter", @{ @"type": @"entered", @"targetId": dropTargetID, @"count": @(MIN(urls.count, 128)) });
    return NSDragOperationCopy;
}
- (NSDragOperation)draggingUpdated:(id<NSDraggingInfo>)sender {
    if (!dropTargetID.length) return NSDragOperationNone;
    dropLeaseDeadline = NSDate.timeIntervalSinceReferenceDate + 1.0;
    return NSDragOperationCopy;
}
- (void)draggingExited:(id<NSDraggingInfo>)sender {
    if (dropTargetID.length) emitDropEvent(@"drop-leave", @{ @"type": @"left", @"targetId": dropTargetID });
    hideDropPanel();
}
- (BOOL)prepareForDragOperation:(id<NSDraggingInfo>)sender { return dropTargetID.length > 0; }
- (BOOL)performDragOperation:(id<NSDraggingInfo>)sender {
    NSString *target = dropTargetID ?: @"";
    NSArray *urls = [[sender draggingPasteboard] readObjectsForClasses:@[NSURL.class]
        options:@{NSPasteboardURLReadingFileURLsOnlyKey:@YES}] ?: @[];
    NSMutableArray *paths = [NSMutableArray array];
    for (NSURL *url in urls) {
        if (paths.count >= 128) break;
        if (url.isFileURL && url.path.length) [paths addObject:url.path];
    }
    if (target.length && paths.count) emitDropEvent(@"drop", @{ @"type": @"dropped", @"targetId": target, @"paths": paths });
    hideDropPanel();
    return paths.count > 0;
}
@end

@interface DynappPresentation : NSObject <NSMenuDelegate>
@end
static DynappPresentation *presenter;

@implementation DynappPresentation
- (void)click:(id)sender {
    NSEvent *event = NSApp.currentEvent;
    dynappGoPresentationEvent("click", event.clickCount > 1 ? "double-click" : "click", 0);
}
- (void)menuWillOpen:(NSMenu *)menu {
    if (menu == statusItem.menu) dynappGoPresentationEvent("click", "click", 0);
}
- (void)menuClick:(NSMenuItem *)item {
    NSString *type = item.representedObject[@"type"];
    if ([type isEqualToString:@"checkbox"]) item.state = item.state == NSControlStateValueOn ? NSControlStateValueOff : NSControlStateValueOn;
    if ([type isEqualToString:@"radio"]) {
        NSArray<NSMenuItem *> *siblings = item.menu.itemArray;
        NSInteger index = [siblings indexOfObject:item];
        for (NSInteger i = index; i >= 0 && [siblings[i].representedObject[@"type"] isEqualToString:@"radio"]; i--) [siblings[i] setState:i == index ? NSControlStateValueOn : NSControlStateValueOff];
        for (NSInteger i = index + 1; i < siblings.count && [siblings[i].representedObject[@"type"] isEqualToString:@"radio"]; i++) [siblings[i] setState:NSControlStateValueOff];
    }
    NSString *identifier = item.representedObject[@"id"] ?: @"";
    dynappGoPresentationEvent("menu", (char *)identifier.UTF8String, 0);
}
@end

static NSMenu *makeMenu(NSArray *spec) {
    NSMenu *menu = [[NSMenu alloc] init];
    menu.autoenablesItems = NO;
    for (NSDictionary *entry in spec) {
        if ([entry[@"type"] isEqualToString:@"separator"]) { [menu addItem:NSMenuItem.separatorItem]; continue; }
        NSMenuItem *item = [[NSMenuItem alloc] initWithTitle:entry[@"label"] ?: @"" action:@selector(menuClick:) keyEquivalent:@""];
        item.target = presenter;
        item.representedObject = entry;
        item.enabled = entry[@"enabled"] ? [entry[@"enabled"] boolValue] : YES;
        item.state = [entry[@"checked"] boolValue] ? NSControlStateValueOn : NSControlStateValueOff;
        if ([entry[@"submenu"] count]) item.submenu = makeMenu(entry[@"submenu"]);
        [menu addItem:item];
    }
    return menu;
}

static OSStatus hotkeyPressed(EventHandlerCallRef handler, EventRef event, void *context) {
    EventHotKeyID identifier;
    OSStatus result = GetEventParameter(event, kEventParamDirectObject, typeEventHotKeyID, NULL, sizeof(identifier), NULL, &identifier);
    if (result == noErr) dynappGoPresentationEvent("hotkey", "", identifier.id);
    return result;
}

void dynapp_presentation_run(void) {
    @autoreleasepool {
        [NSApplication sharedApplication];
        [NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
        presenter = [DynappPresentation new];
        hotkeys = [NSMutableDictionary new];
        EventTypeSpec type = { kEventClassKeyboard, kEventHotKeyPressed };
        InstallApplicationEventHandler(&hotkeyPressed, 1, &type, NULL, NULL);
        [NSApp run];
    }
}

int dynapp_presentation_drop_arm(const char *json) {
    NSData *data = [[NSString stringWithUTF8String:json ?: "{}"] dataUsingEncoding:NSUTF8StringEncoding];
    NSDictionary *bounds = [NSJSONSerialization JSONObjectWithData:data options:0 error:nil];
    if (![bounds isKindOfClass:NSDictionary.class]) return 0;
    __block int ok = 0;
    dispatch_sync(dispatch_get_main_queue(), ^{
        CGFloat width = [bounds[@"width"] doubleValue], height = [bounds[@"height"] doubleValue];
        NSString *identifier = bounds[@"id"];
        if (![identifier isKindOfClass:NSString.class] || !identifier.length || width < 1 || height < 1) return;
        if (!dropPanel) {
            dropPanel = [[NSPanel alloc] initWithContentRect:NSZeroRect
                styleMask:NSWindowStyleMaskBorderless | NSWindowStyleMaskNonactivatingPanel
                backing:NSBackingStoreBuffered defer:NO];
            dropPanel.opaque = NO;
            dropPanel.backgroundColor = [NSColor colorWithSRGBRed:0.10 green:0.48 blue:0.85 alpha:0.18];
            dropPanel.hasShadow = NO;
            dropPanel.level = NSFloatingWindowLevel;
            dropPanel.hidesOnDeactivate = NO;
            dropPanel.collectionBehavior = NSWindowCollectionBehaviorTransient | NSWindowCollectionBehaviorMoveToActiveSpace;
            DynappDropView *view = [[DynappDropView alloc] initWithFrame:NSZeroRect];
            [view registerForDraggedTypes:@[NSPasteboardTypeFileURL]];
            dropPanel.contentView = view;
        }
        CGFloat desktopTop = NSMaxY(NSScreen.screens.firstObject.frame);
        NSRect frame = NSMakeRect([bounds[@"x"] doubleValue],
            desktopTop - [bounds[@"y"] doubleValue] - height, width, height);
        dropTargetID = [identifier copy];
        dropLeaseDeadline = NSDate.timeIntervalSinceReferenceDate + 1.0;
        [dropPanel setFrame:frame display:YES];
        [dropPanel orderFrontRegardless];
        [dropLeaseTimer invalidate];
        dropLeaseTimer = [NSTimer scheduledTimerWithTimeInterval:0.25 repeats:YES block:^(NSTimer *timer) {
            if (dropTargetID.length && NSDate.timeIntervalSinceReferenceDate >= dropLeaseDeadline) hideDropPanel();
        }];
        ok = 1;
    });
    return ok;
}

void dynapp_presentation_drop_hide(void) {
    dispatch_sync(dispatch_get_main_queue(), ^{ hideDropPanel(); });
}

int dynapp_presentation_tray(const char *json) {
    NSData *data = [[NSString stringWithUTF8String:json] dataUsingEncoding:NSUTF8StringEncoding];
    NSDictionary *options = [NSJSONSerialization JSONObjectWithData:data options:0 error:NULL];
    __block int ok = 0;
    dispatch_sync(dispatch_get_main_queue(), ^{
        if (!statusItem) statusItem = [NSStatusBar.systemStatusBar statusItemWithLength:NSVariableStatusItemLength];
        NSImage *icon = [NSImage imageWithSystemSymbolName:@"square.stack.3d.up" accessibilityDescription:@"DynApp"];
        icon.template = YES;
        statusItem.button.image = icon;
        statusItem.button.title = options[@"title"] ?: @"";
        statusItem.button.toolTip = options[@"tooltip"] ?: @"DynApp";
        statusItem.button.target = presenter;
        statusItem.button.action = @selector(click:);
        NSArray *spec = options[@"menu"];
        statusItem.menu = [spec isKindOfClass:NSArray.class] && spec.count ? makeMenu(spec) : nil;
        statusItem.menu.delegate = presenter;
        ok = statusItem != nil;
    });
    return ok;
}
void dynapp_presentation_destroy(void) {
    dispatch_sync(dispatch_get_main_queue(), ^{ if (statusItem) [NSStatusBar.systemStatusBar removeStatusItem:statusItem]; statusItem = nil; });
}
void dynapp_presentation_stop(void) {
    dispatch_async(dispatch_get_main_queue(), ^{ [NSApp stop:nil]; [NSApp postEvent:[NSEvent otherEventWithType:NSEventTypeApplicationDefined location:NSZeroPoint modifierFlags:0 timestamp:0 windowNumber:0 context:nil subtype:0 data1:0 data2:0] atStart:NO]; });
}
int dynapp_presentation_register(unsigned int identifier, unsigned int key, unsigned int modifiers) {
    __block int ok = 0;
    dispatch_sync(dispatch_get_main_queue(), ^{
        UInt32 flags = ((modifiers & 1) ? optionKey : 0) | ((modifiers & 2) ? controlKey : 0) | ((modifiers & 4) ? shiftKey : 0) | ((modifiers & 8) ? cmdKey : 0);
        EventHotKeyRef ref = NULL;
        EventHotKeyID hotkeyID = { 'DyAp', identifier };
        if (RegisterEventHotKey(key, flags, hotkeyID, GetApplicationEventTarget(), kEventHotKeyExclusive, &ref) == noErr) { hotkeys[@(identifier)] = [NSValue valueWithPointer:ref]; ok = 1; }
    });
    return ok;
}
void dynapp_presentation_unregister(unsigned int identifier) {
    dispatch_sync(dispatch_get_main_queue(), ^{ NSValue *value = hotkeys[@(identifier)]; if (value) UnregisterEventHotKey(value.pointerValue); [hotkeys removeObjectForKey:@(identifier)]; });
}
