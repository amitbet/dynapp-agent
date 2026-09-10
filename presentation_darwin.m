//go:build darwin && cgo

#import <AppKit/AppKit.h>
#import <Carbon/Carbon.h>

extern void dynappGoPresentationEvent(char *, char *, unsigned int);
static NSStatusItem *statusItem;
static NSMutableDictionary<NSNumber *, NSValue *> *hotkeys;

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
