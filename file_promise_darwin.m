#import <AppKit/AppKit.h>

extern void dynappGoFilePromiseRequested(char *identifier, char *path);

@interface DynAppLazyFile : NSObject <NSPasteboardItemDataProvider>
@property(nonatomic, copy) NSString *identifier;
@property(nonatomic, copy) NSString *fileName;
@property(nonatomic, copy) NSString *path;
@property(nonatomic, strong) dispatch_semaphore_t completion;
@property(nonatomic, copy) NSString *errorMessage;
@end

static NSArray<DynAppLazyFile *> *DynFiles;
static BOOL DynFilesArmed;
static id DynPasteMonitor;

static NSString *DynAppFinderTarget(void) {
    NSAppleScript *script = [[NSAppleScript alloc] initWithSource:
        @"tell application \"Finder\" to POSIX path of (target of front window as alias)"];
    NSAppleEventDescriptor *result = [script executeAndReturnError:nil];
    NSString *path = result.stringValue;
    return path.length ? path : nil;
}

static NSString *DynAppAvailablePath(NSString *directory, NSString *name) {
    NSString *candidate = [directory stringByAppendingPathComponent:name];
    if (![[NSFileManager defaultManager] fileExistsAtPath:candidate]) return candidate;
    NSString *stem = name.stringByDeletingPathExtension;
    NSString *extension = name.pathExtension;
    for (NSUInteger index = 2; index < 10000; index++) {
        NSString *copyName = extension.length
            ? [NSString stringWithFormat:@"%@ %lu.%@", stem, (unsigned long)index, extension]
            : [NSString stringWithFormat:@"%@ %lu", stem, (unsigned long)index];
        candidate = [directory stringByAppendingPathComponent:copyName];
        if (![[NSFileManager defaultManager] fileExistsAtPath:candidate]) return candidate;
    }
    return [directory stringByAppendingPathComponent:[NSString stringWithFormat:@"%@-%@", NSUUID.UUID.UUIDString, name]];
}

@implementation DynAppLazyFile
- (void)pasteboard:(NSPasteboard *)pasteboard item:(NSPasteboardItem *)item provideDataForType:(NSPasteboardType)type {
    if (!self.path) {
        NSString *root = [NSTemporaryDirectory() stringByAppendingPathComponent:@"dynapp-file-promises"];
        [[NSFileManager defaultManager] createDirectoryAtPath:root withIntermediateDirectories:YES attributes:nil error:nil];
        self.path = [root stringByAppendingPathComponent:[NSString stringWithFormat:@"%@-%@", NSUUID.UUID.UUIDString, self.fileName]];
        self.completion = dispatch_semaphore_create(0);
        dynappGoFilePromiseRequested((char *)self.identifier.UTF8String, (char *)self.path.UTF8String);
        dispatch_semaphore_wait(self.completion, DISPATCH_TIME_FOREVER);
    }
    if (!self.errorMessage.length) [item setString:[NSURL fileURLWithPath:self.path].absoluteString forType:NSPasteboardTypeFileURL];
}
@end

static void DynAppArmFilesForFinder(void) {
    if (!DynFiles.count || DynFilesArmed) return;
    NSString *directory = DynAppFinderTarget();
    if (!directory.length) return;
    DynFilesArmed = YES;
    for (DynAppLazyFile *file in DynFiles) {
        file.path = DynAppAvailablePath(directory, file.fileName);
        dynappGoFilePromiseRequested((char *)file.identifier.UTF8String, (char *)file.path.UTF8String);
    }
}

void dynapp_file_promise_run(void) {
    @autoreleasepool {
        [NSApplication sharedApplication];
        DynPasteMonitor = [NSEvent addGlobalMonitorForEventsMatchingMask:NSEventMaskKeyDown handler:^(NSEvent *event) {
            if (!(event.modifierFlags & NSEventModifierFlagCommand) ||
                ![event.charactersIgnoringModifiers.lowercaseString isEqualToString:@"v"]) return;
            NSRunningApplication *frontmost = NSWorkspace.sharedWorkspace.frontmostApplication;
            if ([frontmost.bundleIdentifier isEqualToString:@"com.apple.finder"]) DynAppArmFilesForFinder();
        }];
        [NSApp run];
    }
}

void dynapp_file_promise_publish(const char *json) {
    NSString *text = [NSString stringWithUTF8String:json ?: "[]"];
    dispatch_async(dispatch_get_main_queue(), ^{
        NSArray *entries = [NSJSONSerialization JSONObjectWithData:[text dataUsingEncoding:NSUTF8StringEncoding] options:0 error:nil];
        NSMutableArray *files = [NSMutableArray array];
        for (NSDictionary *entry in entries) {
            DynAppLazyFile *file = [DynAppLazyFile new];
            file.identifier = entry[@"id"];
            file.fileName = entry[@"name"];
            [files addObject:file];
        }
        DynFiles = files;
        DynFilesArmed = NO;
        NSPasteboard *pasteboard = [NSPasteboard generalPasteboard];
        [pasteboard clearContents];
        [pasteboard setString:@"DynApp remote files" forType:@"com.dynapp.remote-files"];
    });
}

void dynapp_file_promise_complete(const char *identifier, const char *errorMessage) {
    NSString *key = [NSString stringWithUTF8String:identifier ?: ""];
    NSString *message = [NSString stringWithUTF8String:errorMessage ?: ""];
    for (DynAppLazyFile *file in DynFiles) {
        if (![file.identifier isEqualToString:key]) continue;
        file.errorMessage = message;
        if (file.completion) dispatch_semaphore_signal(file.completion);
        break;
    }
}
