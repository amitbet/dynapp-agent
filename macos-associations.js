// Shared by the Go agent and Electron. Executed by macOS JavaScript for Automation.
function run(argv) {
  ObjC.import('CoreServices');
  // Finder's normal Open action can ask Launch Services for different roles
  // depending on the document type. This operation is not an edit request,
  // so accept and verify the handler for all roles.
  var allRoles = 0xffffffff;
  var mode = argv[0];
  var bundleID = argv[1];
  var extensions = JSON.parse(argv[2]);
  var bundlePath = argv[3] || '';
  if (mode !== 'apply' && mode !== 'get') throw new Error('Invalid association operation');
  // JXA owns bridged references for this short-lived process.
  function string(ref) {
    return ObjC.unwrap(ObjC.castRefToObject(ref)) || null;
  }
  function customType(extension) {
    return $(bundleID + '.' + extension);
  }
  function owns(value) {
    return value && value.toLowerCase() === bundleID.toLowerCase();
  }
  var applied = [];
  extensions.forEach(function(extension) {
    // Resolve the OS content type instead of inventing an app-specific UTI.
    var type = $.UTTypeCreatePreferredIdentifierForTag($('public.filename-extension'), $(extension), null);
    if (!string(type)) throw new Error('Cannot resolve content type for .' + extension);
    var preferredType = string(type);
    var custom = customType(extension);
    if (mode === 'apply') {
      var status = $.LSSetDefaultRoleHandlerForContentType(type, allRoles, $(bundleID));
      if (status === -50) status = $.LSSetDefaultRoleHandlerForContentType(custom, allRoles, $(bundleID));
      if (status !== 0) throw new Error('Cannot set default for .' + extension + ' (macOS ' + status + ')');
    }
    var handler = $.LSCopyDefaultRoleHandlerForContentType(type, allRoles);
    var current = string(handler);
    // A custom UTI is only a valid fallback when it is the preferred UTI that
    // Launch Services assigned to this extension. Otherwise Finder will keep
    // using the system UTI and its existing handler (for example, Brave).
    if (!owns(current) && preferredType === custom) current = string($.LSCopyDefaultRoleHandlerForContentType(custom, allRoles));
    // macOS may show a confirmation sheet after the setter returns. Give the
    // user's Use/Keep choice time to update Launch Services before verifying.
    for (var attempt = 0; mode === 'apply' && !owns(current) && attempt < 600; attempt++) {
      $.NSRunLoop.currentRunLoop.runUntilDate($.NSDate.dateWithTimeIntervalSinceNow(0.05));
      current = string($.LSCopyDefaultRoleHandlerForContentType(type, allRoles));
      if (!owns(current) && preferredType === custom) current = string($.LSCopyDefaultRoleHandlerForContentType(custom, allRoles));
    }
    if (owns(current)) applied.push(extension);
    else if (mode === 'apply') throw new Error('macOS kept ' + (current || 'no application') + ' as the default for .' + extension);
    // A request can also be ignored without an OSStatus error. Treat that as
    // an unapplied extension instead of failing the whole selection.
  });
  return JSON.stringify(applied);
}
