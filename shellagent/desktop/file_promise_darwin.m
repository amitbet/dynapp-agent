#import <AppKit/AppKit.h>

extern void dynappGoFilePromiseRequested(char *identifier, char *path);

// Publish an existing placeholder URL immediately. Finder coordinates its
// background copy with this presenter, which supplies the contents on demand.
// Never wait inside a pasteboard callback: Finder reads that on its UI thread.
@interface DynAppClipboardFile : NSObject <NSFilePresenter>
@property(nonatomic, copy) NSString *identifier;
@property(nonatomic, copy) NSString *fileName;
@property(nonatomic, copy) NSString *directory;
@property(nonatomic, copy) NSURL *presentedItemURL;
@property(nonatomic, strong) NSOperationQueue *presentedItemOperationQueue;
@property(nonatomic, strong) NSMutableArray *waiters;
@property(nonatomic, copy) NSError *failure;
@property(nonatomic) int64_t size;
@property(nonatomic) BOOL requested;
@property(nonatomic) BOOL finished;
@property(nonatomic) BOOL retired;
@property(nonatomic) NSUInteger readers;
@property(nonatomic) NSTimeInterval startedAt;
@end

// State and UI belong to the main queue. Presenter callbacks enqueue work and
// return; neither their operation queue nor the pasteboard server waits here.
static NSMutableDictionary<NSString *, DynAppClipboardFile *> *DynFiles;
static NSPanel *DynProgressPanel;
static NSProgressIndicator *DynProgressBar;
static NSTextField *DynProgressName;
static NSTextField *DynProgressDetail;
static NSTimer *DynProgressTimer;
static NSString *DynLastFailure;
static const NSTimeInterval DynTransferTimeoutSeconds = 60 * 60;

static NSString *DynAppPromiseRoot(void) {
    return [NSTemporaryDirectory() stringByAppendingPathComponent:@"dynapp-file-promises"];
}

static void DynUpdateProgress(void);
static void DynCompleteFile(DynAppClipboardFile *file, NSString *errorMessage);

static void DynDiscardRetiredFile(DynAppClipboardFile *file) {
    if (!file.retired || file.readers || file.waiters.count || (file.requested && !file.finished)) return;
    [NSFileCoordinator removeFilePresenter:file];
    [[NSFileManager defaultManager] removeItemAtPath:file.directory error:nil];
    [DynFiles removeObjectForKey:file.identifier];
}

static void DynShowProgress(void) {
    if (!DynProgressPanel) {
        DynProgressPanel = [[NSPanel alloc] initWithContentRect:NSMakeRect(0, 0, 440, 132)
            styleMask:NSWindowStyleMaskTitled | NSWindowStyleMaskClosable | NSWindowStyleMaskNonactivatingPanel
            backing:NSBackingStoreBuffered defer:NO];
        DynProgressPanel.title = @"Downloading clipboard files";
        DynProgressPanel.floatingPanel = YES;
        DynProgressPanel.hidesOnDeactivate = NO;
        DynProgressPanel.releasedWhenClosed = NO;
        DynProgressPanel.level = NSFloatingWindowLevel;
        [DynProgressPanel center];
        DynProgressName = [NSTextField labelWithString:@""];
        DynProgressName.frame = NSMakeRect(20, 90, 400, 22);
        DynProgressName.lineBreakMode = NSLineBreakByTruncatingMiddle;
        DynProgressName.font = [NSFont systemFontOfSize:13 weight:NSFontWeightMedium];
        DynProgressBar = [[NSProgressIndicator alloc] initWithFrame:NSMakeRect(20, 58, 400, 18)];
        DynProgressBar.indeterminate = NO;
        DynProgressBar.minValue = 0;
        DynProgressBar.maxValue = 100;
        DynProgressDetail = [NSTextField labelWithString:@""];
        DynProgressDetail.frame = NSMakeRect(20, 20, 400, 30);
        DynProgressDetail.font = [NSFont systemFontOfSize:12];
        DynProgressDetail.textColor = NSColor.secondaryLabelColor;
        [DynProgressPanel.contentView addSubview:DynProgressName];
        [DynProgressPanel.contentView addSubview:DynProgressBar];
        [DynProgressPanel.contentView addSubview:DynProgressDetail];
    }
    DynLastFailure = nil;
    [DynProgressPanel orderFrontRegardless];
    if (!DynProgressTimer) {
        DynProgressTimer = [NSTimer scheduledTimerWithTimeInterval:0.25 repeats:YES block:^(NSTimer *timer) {
            DynUpdateProgress();
        }];
    }
    DynUpdateProgress();
}

@implementation DynAppClipboardFile
- (void)prepareContents:(void (^)(NSError *))completion {
    dispatch_async(dispatch_get_main_queue(), ^{
        if (self.finished) { completion(self.failure); return; }
        if (self.retired && !self.requested) {
            completion([NSError errorWithDomain:@"io.dynapp.clipboard" code:2
                userInfo:@{NSLocalizedDescriptionKey:@"The clipboard file was replaced. Copy it again to retry."}]);
            return;
        }
        [self.waiters addObject:[completion copy]];
        if (self.requested) return;
        self.requested = YES;
        self.startedAt = NSDate.timeIntervalSinceReferenceDate;
        DynShowProgress();
        dynappGoFilePromiseRequested((char *)self.identifier.UTF8String, (char *)self.presentedItemURL.path.UTF8String);
    });
}

- (void)savePresentedItemChangesWithCompletionHandler:(void (^)(NSError *))completionHandler {
    [self prepareContents:completionHandler];
}

- (void)relinquishPresentedItemToReader:(void (^)(void (^reacquirer)(void)))reader {
    [self prepareContents:^(NSError *error) {
        // On failure the placeholder is removed before the reader is released,
        // so Finder reports an error instead of copying an empty/partial file.
        self.readers++;
        reader(^{
            dispatch_async(dispatch_get_main_queue(), ^{
                self.readers--;
                DynDiscardRetiredFile(self);
            });
        });
    }];
}
@end

static void DynCompleteFile(DynAppClipboardFile *file, NSString *errorMessage) {
    if (!file || file.finished) return;
    file.finished = YES;
    if (errorMessage.length) {
        file.failure = [NSError errorWithDomain:@"io.dynapp.clipboard" code:1
            userInfo:@{NSLocalizedDescriptionKey:errorMessage}];
        DynLastFailure = errorMessage;
        [[NSFileManager defaultManager] removeItemAtURL:file.presentedItemURL error:nil];
    }
    NSArray *waiters = [file.waiters copy];
    [file.waiters removeAllObjects];
    for (void (^waiter)(NSError *) in waiters) waiter(file.failure);
    DynDiscardRetiredFile(file);
}

static void DynUpdateProgress(void) {
    NSUInteger active = 0;
    BOOL unknownSize = NO;
    int64_t downloaded = 0, total = 0;
    NSString *name = nil;
    for (DynAppClipboardFile *file in DynFiles.allValues) {
        if (!file.requested || file.finished) continue;
        if (NSDate.timeIntervalSinceReferenceDate - file.startedAt >= DynTransferTimeoutSeconds) {
            DynCompleteFile(file, @"The clipboard download timed out. Copy the file again to retry.");
            continue;
        }
        active++;
        name = file.fileName;
        unknownSize |= file.size < 0;
        total += MAX(0, file.size);
        NSDictionary *attributes = [[NSFileManager defaultManager] attributesOfItemAtPath:file.presentedItemURL.path error:nil];
        int64_t bytes = [attributes[NSFileSize] longLongValue];
        downloaded += file.size < 0 ? bytes : MIN(file.size, bytes);
    }
    if (!active) {
        [DynProgressTimer invalidate];
        DynProgressTimer = nil;
        if (DynLastFailure.length) {
            DynProgressName.stringValue = @"File download failed";
            DynProgressDetail.stringValue = DynLastFailure;
        } else {
            [DynProgressPanel orderOut:nil];
        }
        return;
    }
    double percent = total > 0 ? 100.0 * downloaded / total : 0;
    DynProgressName.stringValue = active == 1 ? name : [NSString stringWithFormat:@"Downloading %lu files", (unsigned long)active];
    DynProgressBar.doubleValue = percent;
    DynProgressBar.indeterminate = unknownSize;
    if (unknownSize) [DynProgressBar startAnimation:nil];
    else [DynProgressBar stopAnimation:nil];
    DynProgressDetail.stringValue = unknownSize
        ? [NSString stringWithFormat:@"%@ downloaded", [NSByteCountFormatter stringFromByteCount:downloaded countStyle:NSByteCountFormatterCountStyleFile]]
        : [NSString stringWithFormat:@"%@ of %@ · %.0f%%",
        [NSByteCountFormatter stringFromByteCount:downloaded countStyle:NSByteCountFormatterCountStyleFile],
        [NSByteCountFormatter stringFromByteCount:total countStyle:NSByteCountFormatterCountStyleFile], percent];
}

void dynapp_file_promise_run(void) {
    @autoreleasepool {
        [[NSFileManager defaultManager] removeItemAtPath:DynAppPromiseRoot() error:nil];
        [NSApplication sharedApplication];
        [NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
        [NSApp run];
    }
}

static void DynPublishFileEntries(NSArray *entries, NSPasteboard *pasteboard) {
    if (!DynFiles) DynFiles = [NSMutableDictionary dictionary];
    for (DynAppClipboardFile *file in DynFiles.allValues) {
        file.retired = YES;
        DynDiscardRetiredFile(file);
    }
    NSMutableArray<NSURL *> *urls = [NSMutableArray array];
    for (NSDictionary *entry in entries) {
        NSString *identifier = entry[@"id"], *name = entry[@"name"];
        if (![identifier isKindOfClass:NSString.class] || ![name isKindOfClass:NSString.class] || !name.length) continue;
        DynAppClipboardFile *file = [DynAppClipboardFile new];
        file.identifier = identifier;
        file.fileName = name.lastPathComponent;
        file.size = MAX(-1, [entry[@"size"] longLongValue]);
        file.directory = [DynAppPromiseRoot() stringByAppendingPathComponent:NSUUID.UUID.UUIDString];
        NSError *error = nil;
        if (![[NSFileManager defaultManager] createDirectoryAtPath:file.directory withIntermediateDirectories:YES
            attributes:@{NSFilePosixPermissions:@0700} error:&error]) continue;
        file.presentedItemURL = [NSURL fileURLWithPath:[file.directory stringByAppendingPathComponent:file.fileName]];
        if (![[NSFileManager defaultManager] createFileAtPath:file.presentedItemURL.path contents:NSData.data
            attributes:@{NSFilePosixPermissions:@0600}]) {
            [[NSFileManager defaultManager] removeItemAtPath:file.directory error:nil];
            continue;
        }
        file.presentedItemOperationQueue = [NSOperationQueue new];
        file.presentedItemOperationQueue.maxConcurrentOperationCount = 1;
        file.waiters = [NSMutableArray array];
        DynFiles[identifier] = file;
        [NSFileCoordinator addFilePresenter:file];
        [urls addObject:file.presentedItemURL];
    }
    [pasteboard clearContents];
    if (urls.count) [pasteboard writeObjects:urls];
}

void dynapp_file_promise_publish(const char *json) {
    NSString *text = [NSString stringWithUTF8String:json ?: "[]"];
    dispatch_async(dispatch_get_main_queue(), ^{
        NSArray *entries = [NSJSONSerialization JSONObjectWithData:[text dataUsingEncoding:NSUTF8StringEncoding] options:0 error:nil];
        DynPublishFileEntries(entries, [NSPasteboard generalPasteboard]);
    });
}

void dynapp_file_promise_complete(const char *identifier, const char *errorMessage) {
    NSString *key = [NSString stringWithUTF8String:identifier ?: ""];
    NSString *message = [NSString stringWithUTF8String:errorMessage ?: ""];
    dispatch_async(dispatch_get_main_queue(), ^{
        DynCompleteFile(DynFiles[key], message);
        DynUpdateProgress();
    });
}

void dynapp_file_promise_size(const char *identifier, long long size) {
    NSString *key = [NSString stringWithUTF8String:identifier ?: ""];
    dispatch_async(dispatch_get_main_queue(), ^{
        DynAppClipboardFile *file = DynFiles[key];
        if (file && !file.finished && size >= 0) file.size = size;
        DynUpdateProgress();
    });
}
