#import <AppKit/AppKit.h>

extern void dynappGoFilePromiseRequested(char *identifier, char *path);

// Remote files reach the Mac clipboard as ordinary file URLs whose data is
// provided lazily. Finder, Mail, Slack and every other app enable Paste for
// them like local files. The first reader of a URL triggers the transfer: the
// agent streams the remote file into a private temporary folder while that
// reader waits, then the URL is handed out and cached by the pasteboard
// server for later pastes. Nothing is transferred until something asks.
//
// Finder does not accept file promises (NSFilePromiseProvider) from the
// general pasteboard, and a global Cmd+V monitor needs Input Monitoring
// access, so a real file URL is the only route that works without extra
// permissions.
@interface DynAppLazyFile : NSObject <NSPasteboardItemDataProvider>
@property(nonatomic, copy) NSString *identifier;
@property(nonatomic, copy) NSString *fileName;
@property(nonatomic, copy) NSString *directory;
@property(nonatomic, copy) NSString *path;
@property(nonatomic, strong) dispatch_semaphore_t completion;
@property(nonatomic, copy) NSString *errorMessage;
@property(nonatomic) BOOL requested;
@property(nonatomic) BOOL finished;
@end

// The pasteboard server calls the data provider on the main thread and keeps
// the reader waiting until the data is set, so the main queue cannot own any
// state. Everything below is touched only on this serial queue.
static dispatch_queue_t DynStateQueue(void) {
    static dispatch_queue_t queue;
    static dispatch_once_t once;
    dispatch_once(&once, ^{ queue = dispatch_queue_create("io.dynapp.file-promises", DISPATCH_QUEUE_SERIAL); });
    return queue;
}

static NSMutableDictionary<NSString *, DynAppLazyFile *> *DynFiles;
static const int64_t DynTransferTimeoutSeconds = 60 * 60;

static NSString *DynAppPromiseRoot(void) {
    return [NSTemporaryDirectory() stringByAppendingPathComponent:@"dynapp-file-promises"];
}

@implementation DynAppLazyFile
- (void)pasteboard:(NSPasteboard *)pasteboard item:(NSPasteboardItem *)item provideDataForType:(NSPasteboardType)type {
    if (![type isEqualToString:NSPasteboardTypeFileURL]) return;
    __block BOOL start = NO;
    dispatch_sync(DynStateQueue(), ^{
        if (self.requested) return;
        self.requested = YES;
        self.completion = dispatch_semaphore_create(0);
        // Keep the original filename; a private folder per file separates
        // files that share a name without decorating the pasted name.
        self.directory = [DynAppPromiseRoot() stringByAppendingPathComponent:NSUUID.UUID.UUIDString];
        self.path = [self.directory stringByAppendingPathComponent:self.fileName];
        NSError *error = nil;
        if (![[NSFileManager defaultManager] createDirectoryAtPath:self.directory withIntermediateDirectories:YES attributes:nil error:&error]) {
            self.errorMessage = error.localizedDescription ?: @"could not create the clipboard folder";
            self.finished = YES;
            return;
        }
        start = YES;
    });
    if (start) {
        dynappGoFilePromiseRequested((char *)self.identifier.UTF8String, (char *)self.path.UTF8String);
    }
    if (self.completion && dispatch_semaphore_wait(self.completion, dispatch_time(DISPATCH_TIME_NOW, DynTransferTimeoutSeconds * NSEC_PER_SEC)) != 0) {
        dispatch_sync(DynStateQueue(), ^{
            if (!self.finished) { self.errorMessage = @"remote file clipboard transfer timed out"; self.finished = YES; }
        });
    }
    __block NSString *path = nil;
    dispatch_sync(DynStateQueue(), ^{ if (!self.errorMessage.length) path = self.path; });
    if (path) [item setString:[NSURL fileURLWithPath:path].absoluteString forType:type];
}
@end

// Called on the state queue.
static void DynAppDiscardFile(DynAppLazyFile *file) {
    if (file.directory.length) [[NSFileManager defaultManager] removeItemAtPath:file.directory error:nil];
}

void dynapp_file_promise_run(void) {
    @autoreleasepool {
        // Copies left by an earlier helper belong to offers that died with it.
        [[NSFileManager defaultManager] removeItemAtPath:DynAppPromiseRoot() error:nil];
        [NSApplication sharedApplication];
        [NSApp setActivationPolicy:NSApplicationActivationPolicyProhibited];
        [NSApp run];
    }
}

void dynapp_file_promise_publish(const char *json) {
    NSString *text = [NSString stringWithUTF8String:json ?: "[]"];
    dispatch_async(DynStateQueue(), ^{
        NSArray *entries = [NSJSONSerialization JSONObjectWithData:[text dataUsingEncoding:NSUTF8StringEncoding] options:0 error:nil];
        if (!DynFiles) DynFiles = [NSMutableDictionary dictionary];
        // Forget the previous offer and its temporary copies. A file that is
        // still transferring keeps its entry so its completion can land.
        for (NSString *identifier in DynFiles.allKeys) {
            DynAppLazyFile *file = DynFiles[identifier];
            if (file.requested && !file.finished) continue;
            DynAppDiscardFile(file);
            [DynFiles removeObjectForKey:identifier];
        }
        NSMutableArray<NSPasteboardItem *> *items = [NSMutableArray array];
        for (NSDictionary *entry in entries) {
            NSString *identifier = entry[@"id"], *name = entry[@"name"];
            if (![identifier isKindOfClass:NSString.class] || ![name isKindOfClass:NSString.class] || !name.length) continue;
            DynAppLazyFile *file = [DynAppLazyFile new];
            file.identifier = identifier;
            file.fileName = name;
            NSPasteboardItem *item = [NSPasteboardItem new];
            [item setDataProvider:file forTypes:@[NSPasteboardTypeFileURL]];
            DynFiles[identifier] = file;
            [items addObject:item];
        }
        NSPasteboard *pasteboard = [NSPasteboard generalPasteboard];
        [pasteboard clearContents];
        if (items.count) [pasteboard writeObjects:items];
    });
}

void dynapp_file_promise_complete(const char *identifier, const char *errorMessage) {
    NSString *key = [NSString stringWithUTF8String:identifier ?: ""];
    NSString *message = [NSString stringWithUTF8String:errorMessage ?: ""];
    dispatch_async(DynStateQueue(), ^{
        DynAppLazyFile *file = DynFiles[key];
        if (!file || file.finished) return;
        file.finished = YES;
        if (message.length) {
            file.errorMessage = message;
            DynAppDiscardFile(file);
        }
        // The waiting reader still needs the copy; the next offer removes it.
        if (file.completion) dispatch_semaphore_signal(file.completion);
    });
}
