// Native regression tests. Uses a private pasteboard, not the user's clipboard.
// clang -fobjc-arc -framework AppKit scripts/test-file-promises-darwin.m -o /tmp/test-file-promises && /tmp/test-file-promises
#import "../shellagent/desktop/file_promise_darwin.m"

static NSMutableArray *Requests;
void dynappGoFilePromiseRequested(char *identifier, char *path) {
    [Requests addObject:@{ @"id":@(identifier), @"path":@(path) }];
}
static void Check(BOOL ok, NSString *message) {
    if (!ok) { fprintf(stderr, "FAIL: %s\n", message.UTF8String); exit(1); }
}
static void Until(BOOL (^condition)(void)) {
    NSDate *deadline = [NSDate dateWithTimeIntervalSinceNow:5];
    while (!condition() && deadline.timeIntervalSinceNow > 0)
        [[NSRunLoop mainRunLoop] runUntilDate:[NSDate dateWithTimeIntervalSinceNow:0.01]];
    Check(condition(), @"asynchronous operation timed out");
}
static void ReadFile(NSURL *url, void (^completion)(NSData *, NSError *)) {
    dispatch_async(dispatch_get_global_queue(QOS_CLASS_USER_INITIATED,0), ^{
        NSError *error=nil;
        __block NSData *data=nil;
        [[[NSFileCoordinator alloc] initWithFilePresenter:nil] coordinateReadingItemAtURL:url options:0 error:&error byAccessor:^(NSURL *readURL) {
            data=[NSData dataWithContentsOfURL:readURL];
        }];
        dispatch_async(dispatch_get_main_queue(), ^{ completion(data,error); });
    });
}
static NSURL *Offer(NSPasteboard *board, NSString *identifier, NSString *name, int64_t size) {
    DynPublishFileEntries(@[@{@"id":identifier,@"name":name,@"size":@(size)}],board);
    NSArray *urls=[board readObjectsForClasses:@[NSURL.class] options:nil];
    Check(urls.count==1,@"one immediate file URL is on the pasteboard");
    return urls[0];
}
int main(void) { @autoreleasepool {
    [NSApplication sharedApplication]; [NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
    Requests=[NSMutableArray array];
    NSPasteboard *board=[NSPasteboard pasteboardWithUniqueName];
    NSURL *url=Offer(board,@"first",@"test.txt",8);
    Check(Requests.count==0,@"copy and clipboard URL reads must not start a download");
    __block BOOL readDone=NO, heartbeat=NO;
    ReadFile(url, ^(NSData *data,NSError *error){
        Check(!error && [data isEqualToData:[@"complete" dataUsingEncoding:NSUTF8StringEncoding]],@"reader gets complete bytes");
        readDone=YES;
    });
    Until(^BOOL{return Requests.count==1;});
    Check(!readDone,@"coordinated reader waits for the download");
    Check(DynProgressPanel.visible,@"native progress window is visible during transfer");
    [@"comp" writeToURL:url atomically:NO encoding:NSUTF8StringEncoding error:nil];
    DynUpdateProgress();
    Check(DynProgressBar.doubleValue==50,@"progress reflects downloaded bytes");
    dispatch_async(dispatch_get_main_queue(),^{heartbeat=YES;});
    Until(^BOOL{return heartbeat;});
    Check(!readDone,@"main queue remains responsive while the reader waits");
    [@"complete" writeToURL:url atomically:NO encoding:NSUTF8StringEncoding error:nil];
    dynapp_file_promise_complete("first", "");
    Until(^BOOL{return readDone;});
    Check(!DynProgressPanel.visible,@"progress closes when download completes");
    readDone=NO;
    ReadFile(url,^(NSData *data,NSError *error){Check(data.length==8 && !error,@"repeat paste reads complete file");readDone=YES;});
    Until(^BOOL{return readDone;});
    Check(Requests.count==1,@"repeat paste does not download again");

    url=Offer(board,@"failed",@"failed.txt",8);
    readDone=NO;
    ReadFile(url,^(NSData *data,NSError *error){Check(data==nil,@"failed download never copies a placeholder");readDone=YES;});
    Until(^BOOL{return Requests.count==2;});
    dynapp_file_promise_complete("failed","Test transfer failed");
    Until(^BOOL{return readDone;});
    Check(DynProgressPanel.visible && [DynProgressName.stringValue isEqualToString:@"File download failed"],@"download failure stays visible");

    url=Offer(board,@"retired",@"retired.txt",8);
    readDone=NO;
    ReadFile(url,^(NSData *data,NSError *error){Check(data.length==8 && !error,@"copy survives a new clipboard offer");readDone=YES;});
    Until(^BOOL{return Requests.count==3;});
    Offer(board,@"next",@"next.txt",8);
    Check([[NSFileManager defaultManager] fileExistsAtPath:url.path],@"new offer preserves in-flight source");
    [@"complete" writeToURL:url atomically:NO encoding:NSUTF8StringEncoding error:nil];
    dynapp_file_promise_complete("retired", "");
    Until(^BOOL{return readDone;});
    Until(^BOOL{return DynFiles[@"retired"]==nil;});
    Check(Requests.count==3,@"replacement offer remains lazy");
    url=Offer(board,@"unknown",@"unknown.txt",-1);
    Check(Requests.count==3,@"unknown-size offer does not request bytes");
    readDone=NO;
    ReadFile(url,^(NSData *data,NSError *error){Check(data.length==8 && !error,@"deferred size delivers complete bytes");readDone=YES;});
    Until(^BOOL{return Requests.count==4;});
    Check(DynProgressBar.indeterminate,@"unknown size uses indeterminate progress");
    dynapp_file_promise_size("unknown",8);
    Until(^BOOL{return DynFiles[@"unknown"].size==8;});
    [@"comp" writeToURL:url atomically:NO encoding:NSUTF8StringEncoding error:nil];
    DynUpdateProgress();
    Check(!DynProgressBar.indeterminate && DynProgressBar.doubleValue==50,@"metadata enables byte progress");
    [@"complete" writeToURL:url atomically:NO encoding:NSUTF8StringEncoding error:nil];
    dynapp_file_promise_complete("unknown", "");
    Until(^BOOL{return readDone;});
    DynPublishFileEntries(@[],board);
    [board releaseGlobally];
    [DynProgressPanel orderOut:nil];
    puts("PASS: lazy URL publication, deferred reads, native progress, main queue responsiveness, repeat paste, failure, replacement during copy");
    return 0;
}}
