//go:build darwin && cgo

#import <AppKit/AppKit.h>
#import <ScreenCaptureKit/ScreenCaptureKit.h>

int dynapp_screen_supported(void) { if (@available(macOS 14.0, *)) return 1; return 0; }

char *dynapp_screen_displays(void) {
    __block NSData *result;
    dispatch_sync(dispatch_get_main_queue(), ^{
        NSMutableArray *displays = [NSMutableArray new];
        for (NSScreen *screen in NSScreen.screens) {
            NSNumber *identifier = screen.deviceDescription[@"NSScreenNumber"];
            CGFloat scale = screen.backingScaleFactor;
            [displays addObject:@{ @"id":identifier.stringValue, @"name":screen.localizedName,
                @"width":@((int)(screen.frame.size.width * scale)), @"height":@((int)(screen.frame.size.height * scale)),
                @"scaleFactor":@(scale), @"primary":([identifier unsignedIntValue] == CGMainDisplayID() ? @YES : @NO) }];
        }
        result = [NSJSONSerialization dataWithJSONObject:displays options:0 error:NULL];
    });
    if (!result) return NULL;
    return strdup([[NSString alloc] initWithData:result encoding:NSUTF8StringEncoding].UTF8String);
}

int dynapp_screen_capture(unsigned int identifier, int x, int y, int width, int height, void **data, size_t *length, char **error) {
    if (@available(macOS 14.0, *)) {
        if (!CGPreflightScreenCaptureAccess()) {
            // The user must explicitly allow the signed agent in macOS settings.
            dispatch_async(dispatch_get_main_queue(), ^{ CGRequestScreenCaptureAccess(); });
            *error = strdup("Screen Recording permission is required. Allow DynApp Shell Agent in System Settings > Privacy & Security > Screen Recording, then restart the agent and retry.");
            return 0;
        }
        dispatch_semaphore_t finished = dispatch_semaphore_create(0);
        __block NSData *png;
        __block NSString *failure;
        dispatch_async(dispatch_get_main_queue(), ^{
            [SCShareableContent getShareableContentExcludingDesktopWindows:NO onScreenWindowsOnly:YES completionHandler:^(SCShareableContent *content, NSError *contentError) {
                if (contentError) { failure = contentError.localizedDescription; dispatch_semaphore_signal(finished); return; }
                SCDisplay *display = nil;
                for (SCDisplay *candidate in content.displays) if (candidate.displayID == identifier) { display = candidate; break; }
                if (!display) { failure = @"Display is no longer available"; dispatch_semaphore_signal(finished); return; }
                SCContentFilter *filter = [[SCContentFilter alloc] initWithDisplay:display excludingWindows:@[]];
                CGFloat scale = filter.pointPixelScale;
                if (scale <= 0) scale = 1;
                SCStreamConfiguration *configuration = [SCStreamConfiguration new];
                configuration.width = width;
                configuration.height = height;
                configuration.sourceRect = CGRectMake(x / scale, y / scale, width / scale, height / scale);
                configuration.showsCursor = NO;
                [SCScreenshotManager captureImageWithFilter:filter configuration:configuration completionHandler:^(CGImageRef image, NSError *captureError) {
                    if (captureError || !image) failure = captureError.localizedDescription ?: @"Screen capture returned no image";
                    else png = [[[NSBitmapImageRep alloc] initWithCGImage:image] representationUsingType:NSBitmapImageFileTypePNG properties:@{}];
                    dispatch_semaphore_signal(finished);
                }];
            }];
        });
        if (dispatch_semaphore_wait(finished, dispatch_time(DISPATCH_TIME_NOW, 15 * NSEC_PER_SEC))) {
            *error = strdup("Screen capture timed out"); return 0;
        }
        if (!png) { *error = strdup((failure ?: @"Could not encode screenshot").UTF8String); return 0; }
        if (png.length > 8 * 1024 * 1024) { *error = strdup("Screenshot exceeds 8 MiB; choose a smaller region"); return 0; }
        *data = malloc(png.length);
        if (!*data) { *error = strdup("Could not allocate screenshot"); return 0; }
        memcpy(*data, png.bytes, png.length); *length = png.length;
        return 1;
    }
    *error = strdup("Native screenshots require macOS 14 or newer"); return 0;
}
