//go:build windows

#define NOMINMAX
#include <windows.h>
#include <ole2.h>
#include <objidl.h>
#include <shlobj.h>

#include <algorithm>
#include <chrono>
#include <climits>
#include <condition_variable>
#include <cwchar>
#include <future>
#include <map>
#include <memory>
#include <mutex>
#include <new>
#include <string>
#include <vector>

extern "C" void dynappGoFilePromiseRequested(char *identifier, char *path);

namespace {

constexpr UINT WM_DYNAPP_PUBLISH = WM_APP + 0x410;
constexpr UINT WM_DYNAPP_COMPLETE = WM_APP + 0x411;

struct Descriptor {
    std::string id;
    std::wstring name;
    ULONGLONG size = 0;
};

std::mutex g_runMutex;
std::condition_variable g_runReady;
DWORD g_threadId = 0;
bool g_running = false;

UINT g_fileContents = 0;
UINT g_preferredDropEffect = 0;

std::wstring utf8ToWide(const std::string &value) {
    if (value.empty()) return {};
    int length = MultiByteToWideChar(CP_UTF8, MB_ERR_INVALID_CHARS, value.data(), (int)value.size(), nullptr, 0);
    if (length <= 0) length = MultiByteToWideChar(CP_UTF8, 0, value.data(), (int)value.size(), nullptr, 0);
    if (length <= 0) return {};
    std::wstring result(length, L'\0');
    MultiByteToWideChar(CP_UTF8, 0, value.data(), (int)value.size(), result.data(), length);
    return result;
}

std::string wideToUtf8(const std::wstring &value) {
    if (value.empty()) return {};
    int length = WideCharToMultiByte(CP_UTF8, 0, value.data(), (int)value.size(), nullptr, 0, nullptr, nullptr);
    if (length <= 0) return {};
    std::string result(length, '\0');
    WideCharToMultiByte(CP_UTF8, 0, value.data(), (int)value.size(), result.data(), length, nullptr, nullptr);
    return result;
}

bool parseJsonString(const std::string &json, size_t &position, std::string &value) {
    if (position >= json.size() || json[position] != '"') return false;
    ++position;
    value.clear();
    while (position < json.size()) {
        unsigned char ch = (unsigned char)json[position++];
        if (ch == '"') return true;
        if (ch != '\\') {
            value.push_back((char)ch);
            continue;
        }
        if (position >= json.size()) return false;
        char escaped = json[position++];
        switch (escaped) {
        case '"': value.push_back('"'); break;
        case '\\': value.push_back('\\'); break;
        case '/': value.push_back('/'); break;
        case 'b': value.push_back('\b'); break;
        case 'f': value.push_back('\f'); break;
        case 'n': value.push_back('\n'); break;
        case 'r': value.push_back('\r'); break;
        case 't': value.push_back('\t'); break;
        case 'u': {
            if (position + 4 > json.size()) return false;
            unsigned int codepoint = 0;
            for (int i = 0; i < 4; ++i) {
                char digit = json[position++];
                codepoint <<= 4;
                if (digit >= '0' && digit <= '9') codepoint += (unsigned)(digit - '0');
                else if (digit >= 'a' && digit <= 'f') codepoint += (unsigned)(digit - 'a' + 10);
                else if (digit >= 'A' && digit <= 'F') codepoint += (unsigned)(digit - 'A' + 10);
                else return false;
            }
            if (codepoint <= 0x7f) value.push_back((char)codepoint);
            else if (codepoint <= 0x7ff) {
                value.push_back((char)(0xc0 | (codepoint >> 6)));
                value.push_back((char)(0x80 | (codepoint & 0x3f)));
            } else {
                value.push_back((char)(0xe0 | (codepoint >> 12)));
                value.push_back((char)(0x80 | ((codepoint >> 6) & 0x3f)));
                value.push_back((char)(0x80 | (codepoint & 0x3f)));
            }
            break;
        }
        default: return false;
        }
    }
    return false;
}

bool jsonFieldString(const std::string &object, const char *field, std::string &value) {
    std::string needle = std::string("\"") + field + "\"";
    size_t position = object.find(needle);
    if (position == std::string::npos) return false;
    position = object.find(':', position + needle.size());
    if (position == std::string::npos) return false;
    ++position;
    while (position < object.size() && (object[position] == ' ' || object[position] == '\t' || object[position] == '\r' || object[position] == '\n')) ++position;
    return parseJsonString(object, position, value);
}

bool jsonFieldNumber(const std::string &object, const char *field, ULONGLONG &value) {
    std::string needle = std::string("\"") + field + "\"";
    size_t position = object.find(needle);
    if (position == std::string::npos) return false;
    position = object.find(':', position + needle.size());
    if (position == std::string::npos) return false;
    ++position;
    while (position < object.size() && (object[position] == ' ' || object[position] == '\t' || object[position] == '\r' || object[position] == '\n')) ++position;
    if (position >= object.size() || object[position] < '0' || object[position] > '9') return false;
    ULONGLONG result = 0;
    while (position < object.size() && object[position] >= '0' && object[position] <= '9') {
        unsigned digit = (unsigned)(object[position++] - '0');
        if (result > (ULLONG_MAX - digit) / 10) return false;
        result = result * 10 + digit;
    }
    value = result;
    return true;
}

bool parseDescriptors(const std::string &json, std::vector<Descriptor> &result) {
    size_t position = 0;
    while ((position = json.find('{', position)) != std::string::npos) {
        size_t end = position + 1;
        bool quoted = false;
        bool escaped = false;
        for (; end < json.size(); ++end) {
            char ch = json[end];
            if (quoted) {
                if (escaped) escaped = false;
                else if (ch == '\\') escaped = true;
                else if (ch == '"') quoted = false;
            } else if (ch == '"') quoted = true;
            else if (ch == '}') break;
        }
        if (end >= json.size()) return false;
        std::string object = json.substr(position, end - position + 1);
        Descriptor descriptor;
        std::string name;
        ULONGLONG size = 0;
        if (!jsonFieldString(object, "id", descriptor.id) || !jsonFieldString(object, "name", name) || !jsonFieldNumber(object, "size", size)) return false;
        descriptor.name = utf8ToWide(name);
        if (descriptor.id.empty() || descriptor.name.empty()) return false;
        descriptor.size = size;
        result.push_back(std::move(descriptor));
        position = end + 1;
    }
    return !result.empty();
}

class PromiseState {
public:
    PromiseState(std::string identifier, std::wstring fileName, ULONGLONG fileSize)
        : id(std::move(identifier)), name(std::move(fileName)), size(fileSize) {}

    ~PromiseState() {
        if (!path.empty()) DeleteFileW(path.c_str());
    }

    bool request() {
        std::lock_guard<std::mutex> lock(mutex);
        if (requested) return true;
        wchar_t directory[MAX_PATH] = {};
        DWORD length = GetTempPathW(ARRAYSIZE(directory), directory);
        if (!length || length >= ARRAYSIZE(directory)) {
            completed = true;
            error = HRESULT_FROM_WIN32(GetLastError());
            condition.notify_all();
            return false;
        }
        wchar_t temporary[MAX_PATH] = {};
        if (!GetTempFileNameW(directory, L"DYN", 0, temporary)) {
            completed = true;
            error = HRESULT_FROM_WIN32(GetLastError());
            condition.notify_all();
            return false;
        }
        path = temporary;
        requested = true;
        std::string pathUtf8 = wideToUtf8(path);
        dynappGoFilePromiseRequested(const_cast<char *>(id.c_str()), const_cast<char *>(pathUtf8.c_str()));
        return true;
    }

    void complete(const std::string &message) {
        std::lock_guard<std::mutex> lock(mutex);
        completed = true;
        error = message.empty() ? S_OK : E_FAIL;
        condition.notify_all();
    }

    std::string id;
    std::wstring name;
    ULONGLONG size;
    std::wstring path;
    std::mutex mutex;
    std::condition_variable condition;
    bool requested = false;
    bool completed = false;
    HRESULT error = S_OK;
};

class LazyFileStream final : public IStream {
public:
    explicit LazyFileStream(std::shared_ptr<PromiseState> promise) : promise_(std::move(promise)) {}

    ~LazyFileStream() override {
        if (file_ != INVALID_HANDLE_VALUE) CloseHandle(file_);
    }

    HRESULT STDMETHODCALLTYPE QueryInterface(REFIID iid, void **object) override {
        if (!object) return E_POINTER;
        *object = nullptr;
        if (iid == IID_IUnknown || iid == IID_IStream) {
            *object = static_cast<IStream *>(this);
            AddRef();
            return S_OK;
        }
        return E_NOINTERFACE;
    }
    ULONG STDMETHODCALLTYPE AddRef() override { return ++references_; }
    ULONG STDMETHODCALLTYPE Release() override {
        ULONG remaining = --references_;
        if (!remaining) delete this;
        return remaining;
    }

    HRESULT STDMETHODCALLTYPE Read(void *buffer, ULONG bytes, ULONG *read) override {
        if (!read) return E_POINTER;
        *read = 0;
        if (!buffer && bytes) return E_POINTER;
        if (!promise_->request()) return E_FAIL;
        if (position_ >= promise_->size) return S_OK;
        if (!bytes) return S_OK;

        while (true) {
            HRESULT status = S_OK;
            bool finished = false;
            {
                std::lock_guard<std::mutex> lock(promise_->mutex);
                status = promise_->error;
                finished = promise_->completed;
            }
            if (FAILED(status)) return status;

            ULONGLONG available = 0;
            WIN32_FILE_ATTRIBUTE_DATA attributes = {};
            if (GetFileAttributesExW(promise_->path.c_str(), GetFileExInfoStandard, &attributes)) {
                available = ((ULONGLONG)attributes.nFileSizeHigh << 32) | attributes.nFileSizeLow;
            }
            if (available > position_ || (finished && available >= promise_->size)) {
                if (!openFile()) return HRESULT_FROM_WIN32(GetLastError());
                LARGE_INTEGER offset;
                offset.QuadPart = (LONGLONG)position_;
                if (!SetFilePointerEx(file_, offset, nullptr, FILE_BEGIN)) return HRESULT_FROM_WIN32(GetLastError());
                ULONGLONG remaining = promise_->size - position_;
                DWORD requested = (DWORD)min<ULONGLONG>(bytes, min<ULONGLONG>(remaining, 1024 * 1024));
                DWORD actual = 0;
                if (!ReadFile(file_, buffer, requested, &actual, nullptr)) return HRESULT_FROM_WIN32(GetLastError());
                if (actual) {
                    position_ += actual;
                    *read = actual;
                    return S_OK;
                }
            }
            if (finished) return S_OK;
            Sleep(15);
        }
    }

    HRESULT STDMETHODCALLTYPE Write(const void *, ULONG, ULONG *) override { return STG_E_ACCESSDENIED; }

    HRESULT STDMETHODCALLTYPE Seek(LARGE_INTEGER move, DWORD origin, ULARGE_INTEGER *newPosition) override {
        LONGLONG base = 0;
        if (origin == STREAM_SEEK_CUR) base = (LONGLONG)position_;
        else if (origin == STREAM_SEEK_END) base = (LONGLONG)promise_->size;
        else if (origin != STREAM_SEEK_SET) return STG_E_INVALIDFUNCTION;
        LONGLONG next = base + move.QuadPart;
        if (next < 0) return STG_E_INVALIDFUNCTION;
        position_ = (ULONGLONG)next;
        if (newPosition) newPosition->QuadPart = position_;
        return S_OK;
    }

    HRESULT STDMETHODCALLTYPE SetSize(ULARGE_INTEGER) override { return STG_E_ACCESSDENIED; }

    HRESULT STDMETHODCALLTYPE CopyTo(IStream *destination, ULARGE_INTEGER bytes, ULARGE_INTEGER *read, ULARGE_INTEGER *written) override {
        if (!destination) return E_POINTER;
        ULARGE_INTEGER totalRead = {}, totalWritten = {};
        std::vector<unsigned char> buffer(256 * 1024);
        while (totalRead.QuadPart < bytes.QuadPart) {
            ULONG request = (ULONG)min<ULONGLONG>(buffer.size(), bytes.QuadPart - totalRead.QuadPart);
            ULONG got = 0;
            HRESULT result = Read(buffer.data(), request, &got);
            if (FAILED(result)) return result;
            if (!got) break;
            ULONG put = 0;
            result = destination->Write(buffer.data(), got, &put);
            if (FAILED(result)) return result;
            totalRead.QuadPart += got;
            totalWritten.QuadPart += put;
            if (put != got) break;
        }
        if (read) *read = totalRead;
        if (written) *written = totalWritten;
        return S_OK;
    }

    HRESULT STDMETHODCALLTYPE Commit(DWORD) override { return S_OK; }
    HRESULT STDMETHODCALLTYPE Revert() override { return STG_E_REVERTED; }
    HRESULT STDMETHODCALLTYPE LockRegion(ULARGE_INTEGER, ULARGE_INTEGER, DWORD) override { return STG_E_INVALIDFUNCTION; }
    HRESULT STDMETHODCALLTYPE UnlockRegion(ULARGE_INTEGER, ULARGE_INTEGER, DWORD) override { return STG_E_INVALIDFUNCTION; }

    HRESULT STDMETHODCALLTYPE Stat(STATSTG *stat, DWORD flags) override {
        if (!stat) return E_POINTER;
        ZeroMemory(stat, sizeof(*stat));
        stat->type = STGTY_STREAM;
        stat->cbSize.QuadPart = promise_->size;
        stat->grfMode = STGM_READ;
        if (!(flags & STATFLAG_NONAME)) {
            size_t bytes = (promise_->name.size() + 1) * sizeof(wchar_t);
            stat->pwcsName = (LPOLESTR)CoTaskMemAlloc(bytes);
            if (!stat->pwcsName) return E_OUTOFMEMORY;
            CopyMemory(stat->pwcsName, promise_->name.c_str(), bytes);
        }
        return S_OK;
    }

    HRESULT STDMETHODCALLTYPE Clone(IStream **clone) override {
        if (!clone) return E_POINTER;
        *clone = new (std::nothrow) LazyFileStream(promise_);
        if (!*clone) return E_OUTOFMEMORY;
        LARGE_INTEGER offset = {};
        offset.QuadPart = (LONGLONG)position_;
        (*clone)->Seek(offset, STREAM_SEEK_SET, nullptr);
        return S_OK;
    }

private:
    bool openFile() {
        if (file_ != INVALID_HANDLE_VALUE) return true;
        file_ = CreateFileW(promise_->path.c_str(), GENERIC_READ, FILE_SHARE_READ | FILE_SHARE_WRITE | FILE_SHARE_DELETE, nullptr, OPEN_EXISTING, FILE_ATTRIBUTE_NORMAL, nullptr);
        return file_ != INVALID_HANDLE_VALUE;
    }

    LONG references_ = 1;
    std::shared_ptr<PromiseState> promise_;
    HANDLE file_ = INVALID_HANDLE_VALUE;
    ULONGLONG position_ = 0;
};

class LazyDataObject final : public IDataObject {
public:
    explicit LazyDataObject(std::vector<Descriptor> descriptors) {
        for (auto &descriptor : descriptors) {
            promises_.push_back(std::make_shared<PromiseState>(descriptor.id, descriptor.name, descriptor.size));
        }
    }

    const std::vector<std::shared_ptr<PromiseState>> &promises() const { return promises_; }

    HRESULT STDMETHODCALLTYPE QueryInterface(REFIID iid, void **object) override {
        if (!object) return E_POINTER;
        *object = nullptr;
        if (iid == IID_IUnknown || iid == IID_IDataObject) {
            *object = static_cast<IDataObject *>(this);
            AddRef();
            return S_OK;
        }
        return E_NOINTERFACE;
    }
    ULONG STDMETHODCALLTYPE AddRef() override { return ++references_; }
    ULONG STDMETHODCALLTYPE Release() override {
        ULONG remaining = --references_;
        if (!remaining) delete this;
        return remaining;
    }

    HRESULT STDMETHODCALLTYPE GetData(FORMATETC *format, STGMEDIUM *medium) override {
        if (!format || !medium) return E_POINTER;
        ZeroMemory(medium, sizeof(*medium));
        if (format->cfFormat == g_fileContents) {
            if (format->lindex < 0 || (size_t)format->lindex >= promises_.size() || !(format->tymed & TYMED_ISTREAM)) return DV_E_FORMATETC;
            auto *stream = new (std::nothrow) LazyFileStream(promises_[(size_t)format->lindex]);
            if (!stream) return E_OUTOFMEMORY;
            medium->tymed = TYMED_ISTREAM;
            medium->pstm = stream;
            return S_OK;
        }
        if (format->cfFormat == RegisterClipboardFormatW(CFSTR_FILEDESCRIPTORW)) {
            if (!(format->tymed & TYMED_HGLOBAL)) return DV_E_TYMED;
            SIZE_T size = sizeof(UINT) + promises_.size() * sizeof(FILEDESCRIPTORW);
            HGLOBAL memory = GlobalAlloc(GMEM_MOVEABLE | GMEM_ZEROINIT, size);
            if (!memory) return E_OUTOFMEMORY;
            auto *group = (FILEGROUPDESCRIPTORW *)GlobalLock(memory);
            if (!group) { GlobalFree(memory); return E_OUTOFMEMORY; }
            group->cItems = (UINT)promises_.size();
            for (size_t i = 0; i < promises_.size(); ++i) {
                FILEDESCRIPTORW &file = group->fgd[i];
                file.dwFlags = FD_ATTRIBUTES | FD_FILESIZE;
                file.dwFileAttributes = FILE_ATTRIBUTE_NORMAL;
                file.nFileSizeHigh = (DWORD)(promises_[i]->size >> 32);
                file.nFileSizeLow = (DWORD)promises_[i]->size;
                wcsncpy_s(file.cFileName, ARRAYSIZE(file.cFileName), promises_[i]->name.c_str(), _TRUNCATE);
            }
            GlobalUnlock(memory);
            medium->tymed = TYMED_HGLOBAL;
            medium->hGlobal = memory;
            return S_OK;
        }
        if (format->cfFormat == g_preferredDropEffect) {
            if (!(format->tymed & TYMED_HGLOBAL)) return DV_E_TYMED;
            HGLOBAL memory = GlobalAlloc(GMEM_MOVEABLE | GMEM_ZEROINIT, sizeof(DWORD));
            if (!memory) return E_OUTOFMEMORY;
            auto *effect = (DWORD *)GlobalLock(memory);
            if (!effect) { GlobalFree(memory); return E_OUTOFMEMORY; }
            *effect = DROPEFFECT_COPY;
            GlobalUnlock(memory);
            medium->tymed = TYMED_HGLOBAL;
            medium->hGlobal = memory;
            return S_OK;
        }
        return DV_E_FORMATETC;
    }

    HRESULT STDMETHODCALLTYPE GetDataHere(FORMATETC *, STGMEDIUM *) override { return DATA_E_FORMATETC; }
    HRESULT STDMETHODCALLTYPE QueryGetData(FORMATETC *format) override {
        if (!format) return E_POINTER;
        if (format->cfFormat == g_fileContents && format->lindex >= 0 && (size_t)format->lindex < promises_.size() && (format->tymed & TYMED_ISTREAM)) return S_OK;
        if (format->cfFormat == RegisterClipboardFormatW(CFSTR_FILEDESCRIPTORW) && (format->tymed & TYMED_HGLOBAL)) return S_OK;
        if (format->cfFormat == g_preferredDropEffect && (format->tymed & TYMED_HGLOBAL)) return S_OK;
        return DV_E_FORMATETC;
    }
    HRESULT STDMETHODCALLTYPE GetCanonicalFormatEtc(FORMATETC *input, FORMATETC *output) override {
        if (!input || !output) return E_POINTER;
        *output = *input;
        output->ptd = nullptr;
        return DATA_S_SAMEFORMATETC;
    }
    HRESULT STDMETHODCALLTYPE SetData(FORMATETC *format, STGMEDIUM *medium, BOOL release) override {
        if (release && medium) ReleaseStgMedium(medium);
        if (format && (format->cfFormat == RegisterClipboardFormatW(CFSTR_PERFORMEDDROPEFFECT) || format->cfFormat == RegisterClipboardFormatW(CFSTR_PASTESUCCEEDED))) return S_OK;
        return S_OK;
    }
    HRESULT STDMETHODCALLTYPE EnumFormatEtc(DWORD direction, IEnumFORMATETC **enumerator) override {
        if (!enumerator) return E_POINTER;
        *enumerator = nullptr;
        if (direction != DATADIR_GET) return E_NOTIMPL;
        std::vector<FORMATETC> formats;
        formats.push_back({(CLIPFORMAT)RegisterClipboardFormatW(CFSTR_FILEDESCRIPTORW), nullptr, DVASPECT_CONTENT, -1, TYMED_HGLOBAL});
        for (LONG index = 0; index < (LONG)promises_.size(); ++index) formats.push_back({(CLIPFORMAT)g_fileContents, nullptr, DVASPECT_CONTENT, index, TYMED_ISTREAM});
        formats.push_back({(CLIPFORMAT)g_preferredDropEffect, nullptr, DVASPECT_CONTENT, -1, TYMED_HGLOBAL});
        return SHCreateStdEnumFmtEtc((UINT)formats.size(), formats.data(), enumerator);
    }
    HRESULT STDMETHODCALLTYPE DAdvise(FORMATETC *, DWORD, IAdviseSink *, DWORD *) override { return OLE_E_ADVISENOTSUPPORTED; }
    HRESULT STDMETHODCALLTYPE DUnadvise(DWORD) override { return OLE_E_ADVISENOTSUPPORTED; }
    HRESULT STDMETHODCALLTYPE EnumDAdvise(IEnumSTATDATA **enumerator) override { if (enumerator) *enumerator = nullptr; return OLE_E_ADVISENOTSUPPORTED; }

private:
    LONG references_ = 1;
    std::vector<std::shared_ptr<PromiseState>> promises_;
};

std::shared_ptr<LazyDataObject> g_data;
std::map<std::string, std::shared_ptr<PromiseState>> g_promises;

struct PublishMessage {
    std::string json;
    std::promise<HRESULT> result;
};

struct CompleteMessage {
    std::string id;
    std::string error;
};

HRESULT processPublish(const std::string &json) {
    std::vector<Descriptor> descriptors;
    if (!parseDescriptors(json, descriptors)) return E_INVALIDARG;
    auto *rawData = new (std::nothrow) LazyDataObject(std::move(descriptors));
    if (!rawData) return E_OUTOFMEMORY;
    std::shared_ptr<LazyDataObject> data(rawData, [](LazyDataObject *value) { value->Release(); });
    HRESULT result = OleSetClipboard(data.get());
    if (FAILED(result)) return result;
    g_promises.clear();
    for (const auto &promise : data->promises()) g_promises[promise->id] = promise;
    g_data = std::move(data);
    return S_OK;
}

void processComplete(const CompleteMessage &message) {
    auto found = g_promises.find(message.id);
    if (found != g_promises.end()) found->second->complete(message.error);
}

bool waitForHelperThread() {
    std::unique_lock<std::mutex> lock(g_runMutex);
    return g_runReady.wait_for(lock, std::chrono::seconds(5), [] { return g_running; });
}

} // namespace

extern "C" int dynapp_file_promise_publish(const char *json) {
    if (!json || !waitForHelperThread()) return 0;
    auto *message = new (std::nothrow) PublishMessage;
    if (!message) return 0;
    message->json = json;
    std::future<HRESULT> result = message->result.get_future();
    DWORD threadId;
    {
        std::lock_guard<std::mutex> lock(g_runMutex);
        threadId = g_threadId;
    }
    if (!PostThreadMessageW(threadId, WM_DYNAPP_PUBLISH, 0, (LPARAM)message)) {
        delete message;
        return 0;
    }
    return SUCCEEDED(result.get()) ? 1 : 0;
}

extern "C" void dynapp_file_promise_complete(const char *identifier, const char *errorMessage) {
    if (!identifier || !waitForHelperThread()) return;
    auto *message = new (std::nothrow) CompleteMessage;
    if (!message) return;
    message->id = identifier;
    if (errorMessage) message->error = errorMessage;
    DWORD threadId;
    {
        std::lock_guard<std::mutex> lock(g_runMutex);
        threadId = g_threadId;
    }
    if (!PostThreadMessageW(threadId, WM_DYNAPP_COMPLETE, 0, (LPARAM)message)) delete message;
}

extern "C" void dynapp_file_promise_run(void) {
    CoInitializeEx(nullptr, COINIT_APARTMENTTHREADED);
    g_fileContents = RegisterClipboardFormatW(CFSTR_FILECONTENTS);
    g_preferredDropEffect = RegisterClipboardFormatW(CFSTR_PREFERREDDROPEFFECT);
    MSG message;
    PeekMessageW(&message, nullptr, WM_USER, WM_USER, PM_NOREMOVE);
    {
        std::lock_guard<std::mutex> lock(g_runMutex);
        g_threadId = GetCurrentThreadId();
        g_running = true;
    }
    g_runReady.notify_all();
    while (GetMessageW(&message, nullptr, 0, 0) > 0) {
        if (message.message == WM_DYNAPP_PUBLISH) {
            auto *publish = (PublishMessage *)message.lParam;
            HRESULT result = processPublish(publish->json);
            publish->result.set_value(result);
            delete publish;
        } else if (message.message == WM_DYNAPP_COMPLETE) {
            auto *complete = (CompleteMessage *)message.lParam;
            processComplete(*complete);
            delete complete;
        } else {
            TranslateMessage(&message);
            DispatchMessageW(&message);
        }
    }
    {
        std::lock_guard<std::mutex> lock(g_runMutex);
        g_running = false;
        g_threadId = 0;
    }
    g_data.reset();
    g_promises.clear();
    OleSetClipboard(nullptr);
    CoUninitialize();
}
