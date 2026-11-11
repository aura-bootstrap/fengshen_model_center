// Native rolling-window decoder. One context and one state per request.
// Dependencies: whisper.cpp 4979e04 (MIT), nlohmann/json 55f93686 (MIT).
#include "whisper.h"
#include "json.hpp"
#include <algorithm>
#include <atomic>
#include <cmath>
#include <condition_variable>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <deque>
#include <iostream>
#include <memory>
#include <mutex>
#include <regex>
#include <set>
#include <stdexcept>
#include <string>
#include <thread>
#include <vector>
#ifdef _WIN32
#include <fcntl.h>
#include <io.h>
#define NOMINMAX
#include <windows.h>
#endif
using J = nlohmann::json;
static constexpr const char * protocol = "fengshen.whispercpp.stream/v1";
static constexpr size_t max_line = 1u << 20;
static constexpr int rate = 16000;
static std::atomic<bool> cancelled{false};
struct Failure : std::runtime_error { using std::runtime_error::runtime_error; };
static void require(bool ok, const char * code) { if (!ok) throw Failure(code); }
static void keys(const J & j, std::initializer_list<const char *> allowed) {
    require(j.is_object(), "message_decode");
    for (auto it = j.begin(); it != j.end(); ++it) {
        bool found = false;
        for (auto k : allowed) if (it.key() == k) found = true;
        require(found, "unknown_field");
    }
}
static double number(const J & j, const char * k) {
    require(j.contains(k) && j[k].is_number(), "number_type");
    double v = j[k].get<double>(); require(std::isfinite(v), "number_nonfinite"); return v;
}
static uint64_t integer(const J & j, const char * k) {
    require(j.contains(k) && (j[k].is_number_unsigned() || j[k].is_number_integer()), "integer_type");
    require(j[k].get<double>() >= 0, "integer_range"); return j[k].get<uint64_t>();
}
static std::string str(const J & j, const char * k) {
    require(j.contains(k) && j[k].is_string(), "string_type"); return j[k].get<std::string>();
}
static void send(const J & j) {
    auto data = j.dump(); require(data.size()+1 <= max_line, "output_limit");
    require(std::fwrite(data.data(), 1, data.size(), stdout) == data.size() &&
            std::fputc('\n', stdout) != EOF && std::fflush(stdout) == 0, "output_write");
}
// The reader never buffers an unbounded pipe. A single pending frame is the only
// read-ahead; OS pipe capacity supplies backpressure. EOF cancels in-flight ggml.
struct Input {
    std::mutex mu; std::condition_variable cv; std::deque<std::string> pending;
    bool done = false; std::string error; std::thread reader;
    Input() : reader([this] { read(); }) {}
    void read() {
        bool final_seen = false;
        for (;;) {
            std::string line;
            for (;;) {
                int c = std::fgetc(stdin);
                if (c == EOF) {
                    std::lock_guard<std::mutex> lock(mu); done = true;
                    if (!final_seen) cancelled = true;
                    cv.notify_all(); return;
                }
                if (line.size()+1 > max_line) {
                    std::lock_guard<std::mutex> lock(mu); error = "message_too_large";
                    cancelled = true; done = true; cv.notify_all(); return;
                }
                if (c == '\n') break;
                line.push_back(static_cast<char>(c));
            }
            // Parsing here is only for EOF semantics; validation remains on the worker.
            auto probe = J::parse(line, nullptr, false);
            if (probe.is_object() && probe.contains("type") && probe["type"].is_string() && probe["type"] == "finalize") final_seen = true;
            std::unique_lock<std::mutex> lock(mu);
            cv.wait(lock, [&] { return pending.empty(); });
            pending.push_back(std::move(line)); cv.notify_all();
            if (final_seen) { done = true; cv.notify_all(); return; }
        }
    }
    J next() {
        std::unique_lock<std::mutex> lock(mu);
        cv.wait(lock, [&] { return !pending.empty() || done; });
        if (pending.empty()) throw Failure(error.empty() ? "unexpected_eof" : error);
        auto line = std::move(pending.front()); pending.pop_front(); cv.notify_all(); lock.unlock();
        // Reject duplicate object keys, including nested options.
        std::vector<std::set<std::string>> seen;
        auto cb = [&](int, J::parse_event_t event, J & value) {
            if (event == J::parse_event_t::object_start) seen.emplace_back();
            if (event == J::parse_event_t::key) require(seen.back().insert(value.get<std::string>()).second, "duplicate_field");
            if (event == J::parse_event_t::object_end) seen.pop_back();
            return true;
        };
        return J::parse(line, cb);
    }
    // main uses _Exit after flushing: no blocked stdin thread is ever joined on error.
};
struct Options {
    double window = 30, step = 2, tolerance = 0, delay = 0; int rounds = 2;
    explicit Options(const J & j) {
        keys(j, {"window_sec", "step_sec", "stable_rounds", "timestamp_tolerance_sec", "finalize_delay_sec"});
        if (j.contains("window_sec")) window = number(j, "window_sec");
        if (j.contains("step_sec")) step = number(j, "step_sec");
        if (j.contains("stable_rounds")) {
            auto n=integer(j, "stable_rounds"); require(n<=5,"options_range"); rounds=static_cast<int>(n);
        }
        if (j.contains("timestamp_tolerance_sec")) tolerance = number(j, "timestamp_tolerance_sec");
        if (j.contains("finalize_delay_sec")) delay = number(j, "finalize_delay_sec");
        if (window == 0) window = 30;
        if (step == 0) step = 2;
        if (rounds == 0) rounds = 2;
        require(window >= 5 && window <= 120 && step >= .5 && step <= 10 && step <= window &&
                rounds >= 1 && rounds <= 5 && tolerance >= 0 && tolerance <= 2 && delay >= 0 && delay <= 10, "options_range");
    }
};
static std::vector<float> pcm(const std::string & b64, size_t count) {
    require(count > 0 && count <= 320000, "sample_count_range");
    require(b64.size() == ((count*2+2)/3)*4, "pcm_size");
    const std::string alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
    std::vector<uint8_t> bytes; bytes.reserve(count*2);
    for (size_t i = 0; i < b64.size(); i += 4) {
        uint32_t v = 0; int pad = 0;
        for (size_t k = 0; k < 4; ++k) {
            char c = b64[i+k];
            if (c == '=') { require(i+4 == b64.size() && k >= 2, "pcm_base64"); ++pad; v <<= 6; }
            else { auto n = alphabet.find(c); require(n != std::string::npos && pad == 0, "pcm_base64"); v = (v << 6) | static_cast<uint32_t>(n); }
        }
        require(pad <= 2 && (pad != 1 || (v & 255) == 0) && (pad != 2 || (v & 65535) == 0), "pcm_base64");
        bytes.push_back(static_cast<uint8_t>(v >> 16));
        if (pad < 2) bytes.push_back(static_cast<uint8_t>(v >> 8));
        if (pad < 1) bytes.push_back(static_cast<uint8_t>(v));
    }
    require(bytes.size() == count*2, "pcm_size");
    std::vector<float> samples(count);
    for (size_t i = 0; i < count; ++i) samples[i] = static_cast<int16_t>(bytes[2*i] | (bytes[2*i+1] << 8))/32768.f;
    return samples;
}
static std::string compact(const std::string & text) {
    // Match Go strings.Fields / unicode.IsSpace exactly for valid UTF-8.
    std::string s;
    for (size_t i=0; i<text.size();) {
        size_t begin=i; uint32_t c=static_cast<unsigned char>(text[i++]);
        int extra=0;
        if (c>=0xf0) { c &= 7; extra=3; }
        else if (c>=0xe0) { c &= 15; extra=2; }
        else if (c>=0xc0) { c &= 31; extra=1; }
        for (int k=0; k<extra && i<text.size(); ++k) c=(c<<6)|(static_cast<unsigned char>(text[i++])&63);
        bool space=(c>=9 && c<=13) || c==32 || c==0x85 || c==0xa0 || c==0x1680 ||
                   (c>=0x2000 && c<=0x200a) || c==0x2028 || c==0x2029 || c==0x202f || c==0x205f || c==0x3000;
        if (!space) s.append(text,begin,i-begin);
    }
    return s;
}
static bool equal(const J & a, const J & b, double tolerance) {
    if (compact(a["text"]) != compact(b["text"]) || a["words"].size() != b["words"].size()) return false;
    auto times = [&](const J & x, const J & y) {
        return std::abs(x["start_sec"].get<double>()-y["start_sec"].get<double>()) <= tolerance &&
               std::abs(x["end_sec"].get<double>()-y["end_sec"].get<double>()) <= tolerance;
    };
    if (!times(a,b)) return false;
    for (size_t i=0; i<a["words"].size(); ++i)
        if (compact(a["words"][i]["text"]) != compact(b["words"][i]["text"]) || !times(a["words"][i],b["words"][i])) return false;
    return true;
}
struct VADRange { int64_t start, end; };
static VADRange vad_range(float t0, float t1, int64_t count, int64_t previous_end) {
    require(std::isfinite(t0) && std::isfinite(t1) && t0 >= 0 && t1 >= t0, "vad_timeline");
    // Pinned upstream getters return centiseconds, not seconds or samples.
    auto sample = [count](float t) { return static_cast<int64_t>(std::min(static_cast<double>(count), static_cast<double>(t)*rate/100)); };
    int64_t start = std::max(previous_end, sample(t0));
    return {start, std::max(start, sample(t1))};
}
static bool vad_token_time(VADRange range, int64_t base, int64_t total, int64_t t0, int64_t t1,
                           double locked_end, double previous_end, double & start, double & end) {
    require(t0 >= 0 && t1 >= t0, "token_timeline");
    start = static_cast<double>(base+range.start)/rate + t0*.01;
    end = std::min({static_cast<double>(base+range.start)/rate + t1*.01,
                    static_cast<double>(base+range.end)/rate, static_cast<double>(total)/rate});
    if (start < locked_end) return false;
    start = std::max(start, previous_end);
    return start < end;
}
// Distinguish a valid prefix from malformed bytes without replacing either.
enum class UTF8 { valid, incomplete, invalid };
static UTF8 utf8_status(const std::string & s) {
    for (size_t i=0; i<s.size();) {
        auto c = static_cast<unsigned char>(s[i++]);
        if (c < 0x80) continue;
        int n; uint32_t value, minimum;
        if (c >= 0xc2 && c <= 0xdf) { n=1; value=c&31; minimum=0x80; }
        else if (c >= 0xe0 && c <= 0xef) { n=2; value=c&15; minimum=0x800; }
        else if (c >= 0xf0 && c <= 0xf4) { n=3; value=c&7; minimum=0x10000; }
        else return UTF8::invalid;
        for (int k=0; k<n; ++k) {
            if (i == s.size()) return UTF8::incomplete;
            auto b = static_cast<unsigned char>(s[i++]);
            if ((b&0xc0) != 0x80) return UTF8::invalid;
            if (k == 0 && ((c == 0xe0 && b < 0xa0) || (c == 0xed && b >= 0xa0) ||
                           (c == 0xf0 && b < 0x90) || (c == 0xf4 && b >= 0x90))) return UTF8::invalid;
            value=(value<<6)|(b&63);
        }
        if (value < minimum || value > 0x10ffff || (value >= 0xd800 && value <= 0xdfff)) return UTF8::invalid;
    }
    return UTF8::valid;
}
struct TokenAccumulator {
    J words = J::array(); std::string text, pending; double pending_start = -1, pending_end = -1;
    bool use_vad; VADRange range; int64_t base, total; double locked_end; double & candidate_end;
    std::string fragment; int64_t fragment_start = 0, fragment_end = 0;
    void add(const std::string & piece, int64_t t0, int64_t t1) {
        if (piece.empty()) return;
        // Energy alignment can shrink t1 to the next token's t0, which may fall
        // before t0. Such a token has no placeable interval; drop it alone.
        if (t0 >= 0 && t1 < t0) return;
        require(t0 >= 0, "token_timeline");
        bool continuing = !fragment.empty();
        if (!continuing) { fragment_start = t0; fragment_end = t1; }
        fragment += piece; fragment_end = std::max(fragment_end, t1);
        auto status = utf8_status(fragment);
        if (status == UTF8::invalid) {
            fragment.clear();
            // A malformed old group must not swallow the next independent token.
            if (continuing) add(piece, t0, t1);
            return;
        }
        if (status == UTF8::incomplete) return;
        std::string group = std::move(fragment); fragment.clear();
        t0 = fragment_start; t1 = fragment_end;
        // Clip complete byte groups, not their zero-duration continuation tokens.
        double start = static_cast<double>(base)/rate + t0*.01;
        double end = static_cast<double>(base)/rate + t1*.01;
        if (use_vad && !vad_token_time(range, base, total, t0, t1, locked_end, candidate_end, start, end)) return;
        if (start < locked_end) return;
        start = std::max(start, candidate_end);
        end = std::min(end, static_cast<double>(total)/rate);
        if (start >= end) return;
        if (pending_start < 0) pending_start = start;
        pending += group; pending_end = end;
        if (!compact(pending).empty()) {
            if (!words.empty()) pending_start = std::max(pending_start, words.back()["end_sec"].get<double>());
            if (pending_end > pending_start) {
                words.push_back({{"start_sec",pending_start},{"end_sec",pending_end},{"text",pending}});
                candidate_end = pending_end;
                text += pending; pending.clear(); pending_start = -1;
            }
        }
    }
    void finish(J & next) {
        // Only validated whitespace can remain here; incomplete fragments are not published.
        if (!pending.empty() && utf8_status(pending) == UTF8::valid && !words.empty()) {
            words.back()["text"] = words.back()["text"].get<std::string>()+pending; text += pending;
        }
        if (!words.empty()) next.push_back({{"start_sec",words.front()["start_sec"]},{"end_sec",words.back()["end_sec"]},{"text",text},{"words",words}});
    }
};
static void preserve_candidates(J & next, const J & previous, double tolerance) {
    for (size_t i=0; i<next.size() && i<previous.size(); ++i) {
        if (!equal(previous[i], next[i], tolerance)) break;
        if (i+1<next.size() && previous[i]["end_sec"].get<double>() > next[i+1]["start_sec"].get<double>()) break;
        next[i] = previous[i];
    }
}
struct Decoder {
    std::unique_ptr<whisper_context, decltype(&whisper_free)> ctx{nullptr, whisper_free};
    std::unique_ptr<whisper_state, decltype(&whisper_free_state)> state{nullptr, whisper_free_state};
    std::unique_ptr<whisper_vad_context, decltype(&whisper_vad_free)> vad{nullptr, whisper_vad_free};
    Options options; int threads; int64_t base = 0, total = 0, decoded = 0;
    std::vector<float> audio; J history = J::array(), suffix = J::array(); std::vector<int> stable;
    double locked_end = 0;
    Decoder(const std::string & model, int n, Options o, const std::string & vad_model = "") : options(o), threads(n) {
        // Prefer the GPU backend. A missing or unusable Vulkan device must not
        // fail the request, so fall back to CPU with the same thread budget.
        if (!load(model, true)) require(load(model, false), "model_load");
        if (!vad_model.empty()) {
            require(!cancelled, "cancelled");
            auto vp = whisper_vad_default_context_params(); vp.use_gpu = false; vp.n_threads = n;
            vad.reset(whisper_vad_init_from_file_with_params(vad_model.c_str(), vp));
            require(!cancelled, "cancelled"); require(vad != nullptr, "vad_model_load");
        }
    }
    bool load(const std::string & model, bool gpu) {
        auto cp = whisper_context_default_params(); cp.use_gpu = gpu;
        ctx.reset(whisper_init_from_file_with_params_no_state(model.c_str(), cp));
        if (!ctx) return false;
        state.reset(whisper_init_state(ctx.get()));
        return state != nullptr;
    }
    void append(const std::vector<float> & samples) {
        int64_t desired = std::max<int64_t>(0, total+samples.size()-static_cast<int64_t>(options.window*rate));
        // Never evict an unresolved suffix. Fail closed rather than inventing text.
        if (desired > base) {
            require(suffix.empty() || desired <= static_cast<int64_t>(suffix[0]["start_sec"].get<double>()*rate), "window_unstable");
            auto remove = static_cast<size_t>(desired-base);
            require(remove <= audio.size(), "window_chunk_limit");
            audio.erase(audio.begin(), audio.begin()+remove); base = desired;
        }
        audio.insert(audio.end(), samples.begin(), samples.end()); total += samples.size();
        require(audio.size() <= static_cast<size_t>(options.window*rate)+1, "window_limit");
    }
    void decode(bool final) {
        if (audio.empty() || (!final && total-decoded < static_cast<int64_t>(options.step*rate))) return;
        require(!cancelled, "cancelled");
        auto p = whisper_full_default_params(WHISPER_SAMPLING_GREEDY);
        p.n_threads = threads; p.no_context = true; p.n_max_text_ctx = 224;
        // Re-decoding overlapping audio must not feed its own overlapping text
        // back as a prompt. Carry only immutable text preceding the audio window.
        std::string prompt;
        for (const auto & item : history)
            if (item["end_sec"].get<double>() <= static_cast<double>(base)/rate)
                prompt += item["text"].get<std::string>();
        if (prompt.size()>2048) {
            size_t cut=prompt.size()-2048;
            while (cut<prompt.size() && (static_cast<unsigned char>(prompt[cut])&0xc0)==0x80) ++cut;
            prompt.erase(0,cut);
        }
        p.initial_prompt = prompt.empty() ? nullptr : prompt.c_str();
        p.print_special = p.print_progress = p.print_realtime = p.print_timestamps = false;
        p.token_timestamps = true; p.split_on_word = true; p.max_len = 48; p.language = "auto";
        p.temperature = 0; p.temperature_inc = 0;
        p.abort_callback = [](void *) { return cancelled.load(); };
        std::vector<VADRange> ranges;
        if (vad) {
            require(!cancelled, "cancelled");
            std::unique_ptr<whisper_vad_segments, decltype(&whisper_vad_free_segments)> segments(
                whisper_vad_segments_from_samples(vad.get(), whisper_vad_default_params(), audio.data(), static_cast<int>(audio.size())), whisper_vad_free_segments);
            require(!cancelled, "cancelled"); require(segments != nullptr, "vad_failed");
            int64_t end = 0;
            for (int i=0; i<whisper_vad_segments_n_segments(segments.get()); ++i) {
                auto r = vad_range(whisper_vad_segments_get_segment_t0(segments.get(),i), whisper_vad_segments_get_segment_t1(segments.get(),i), audio.size(), end);
                if (r.end > r.start) ranges.push_back(r);
                end = r.end;
            }
        } else ranges.push_back({0, static_cast<int64_t>(audio.size())});
        J next = J::array(); double candidate_end = locked_end;
        for (auto range : ranges) {
        require(!cancelled, "cancelled");
        require(whisper_full_with_state(ctx.get(), state.get(), p, audio.data()+range.start, static_cast<int>(range.end-range.start)) == 0, cancelled ? "cancelled" : "decode_failed");
        require(!cancelled, "cancelled");
        for (int s = 0; s < whisper_full_n_segments_from_state(state.get()); ++s) {
            TokenAccumulator tokens{J::array(), "", "", -1, -1, static_cast<bool>(vad), range, base, total, locked_end, candidate_end};
            for (int t = 0; t < whisper_full_n_tokens_from_state(state.get(), s); ++t) {
                auto td = whisper_full_get_token_data_from_state(state.get(), s, t);
                if (td.id >= whisper_token_eot(ctx.get())) continue;
                std::string piece = whisper_full_get_token_text_from_state(ctx.get(), state.get(), s, t);
                tokens.add(piece, td.t0, td.t1);
            }
            tokens.finish(next);
        }
        }
        require(!cancelled, "cancelled"); decoded = total;
        // Preserve candidate identity exactly when equivalent. This also avoids
        // probability drift once Go has published an item.
        preserve_candidates(next, suffix, options.tolerance);
        suffix = std::move(next);
        require(history.size()+suffix.size() <= 4096, "history_limit");
    }
    J hypothesis() const { J result = history; for (const auto & i : suffix) result.push_back(i); return result; }
    void observe(const J & previous) {
        std::vector<int> rounds;
        for (size_t i=0; i<suffix.size(); ++i) rounds.push_back(i<previous.size() && i<stable.size() && equal(previous[i],suffix[i],options.tolerance) ? stable[i]+1 : 1);
        size_t count=0;
        while (count<suffix.size() && rounds[count]>=options.rounds && suffix[count]["end_sec"].get<double>()+options.delay <= static_cast<double>(total)/rate+1e-9) ++count;
        for (size_t i=0; i<count; ++i) { history.push_back(suffix[i]); locked_end=std::max(locked_end,suffix[i]["end_sec"].get<double>()); }
        suffix.erase(suffix.begin(),suffix.begin()+count); rounds.erase(rounds.begin(),rounds.begin()+count); stable=std::move(rounds);
    }
};
static J envelope(const char * type, const std::string & id, int episode, uint64_t revision) {
    return J{{"protocol",protocol},{"type",type},{"request_id",id},{"episode_number",episode},{"revision",revision}};
}
static void quiet(enum ggml_log_level, const char *, void *) {}
// Logical processors over-subscribe hybrid CPUs and collapse decoder throughput;
// physical cores are the measured ceiling.
static int default_threads() {
#ifdef _WIN32
    DWORD len = 0;
    GetLogicalProcessorInformationEx(RelationProcessorCore, nullptr, &len);
    std::vector<char> buf(len);
    if (len > 0 && GetLogicalProcessorInformationEx(RelationProcessorCore, reinterpret_cast<PSYSTEM_LOGICAL_PROCESSOR_INFORMATION_EX>(buf.data()), &len)) {
        int cores = 0;
        for (DWORD at = 0; at < len;) {
            auto * info = reinterpret_cast<PSYSTEM_LOGICAL_PROCESSOR_INFORMATION_EX>(buf.data() + at);
            if (info->Size == 0) break;
            ++cores; at += info->Size;
        }
        if (cores > 0) return std::min(256, cores);
    }
#endif
    return static_cast<int>(std::min(256u, std::max(1u, std::thread::hardware_concurrency())));
}
int main(int argc, char ** argv) {
#ifdef _WIN32
    _setmode(_fileno(stdin), _O_BINARY); _setmode(_fileno(stdout), _O_BINARY);
    SetErrorMode(SEM_FAILCRITICALERRORS | SEM_NOGPFAULTERRORBOX);
#endif
    whisper_log_set(quiet, nullptr);
    std::string id = "native-error"; int episode=0; uint64_t revision=0;
    int result = 1;
    try {
        if (argc==2 && std::string(argv[1])=="--metadata") {
            send({{"protocol",protocol},{"native",true},{"component","whisper-stream"},{"source_commit","4979e04f5dcaccb36057e059bbaed8a2f5288315"},{"vad_supported",true},
#ifdef WHISPER_STREAM_VULKAN
                  {"gpu_backend","vulkan"},
#endif
                  {"max_message_bytes",max_line},{"max_duration_sec",7200}});
            std::_Exit(0);
        }
        require(argc>=4 && std::string(argv[1])=="--fengshen-stream-v1", "arguments");
        std::string model, vad_model; int threads=default_threads(); bool have_threads=false, use_vad=false;
        for (int i=2; i<argc; ++i) {
            std::string a=argv[i];
            if (a=="--model" && model.empty() && i+1<argc) model=argv[++i];
            else if (a=="--threads" && !have_threads && i+1<argc) {
                std::string n=argv[++i]; require(std::regex_match(n,std::regex("[0-9]{1,3}")), "arguments");
                threads=std::stoi(n); have_threads=true; require(threads>=1 && threads<=256,"arguments");
            } else if (a=="--vad" && !use_vad) use_vad=true;
            else if (a=="--vad-model" && vad_model.empty() && i+1<argc) vad_model=argv[++i];
            else throw Failure("arguments");
        }
        require(!model.empty() && use_vad == !vad_model.empty(),"arguments");
        auto input = new Input(); // Process-owned; terminal paths use _Exit after decoder RAII.
        auto start = input->next();
        keys(start,{"protocol","type","request_id","episode_number","duration_sec","format","options"});
        require(str(start,"protocol")==protocol && str(start,"type")=="start", "start_required");
        id=str(start,"request_id"); require(std::regex_match(id,std::regex("[A-Za-z0-9][A-Za-z0-9._-]{0,127}")),"request_identity");
        auto ep=integer(start,"episode_number"); require(ep>0 && ep<=2147483647,"episode_range"); episode=static_cast<int>(ep);
        require(str(start,"format")=="s16le/16000/1","audio_format");
        double duration=number(start,"duration_sec"); require(duration>0 && duration<=7200,"duration_range");
        int64_t expected=static_cast<int64_t>(std::llround(duration*rate));
        require(std::abs(duration*rate-static_cast<double>(expected))<=1e-5,"duration_samples");
        Options options(start.value("options",J::object()));
        Decoder decoder(model,threads,options,vad_model);
        auto response=[&](const char * type, uint64_t rev) { return envelope(type,id,episode,rev); };
        send(response("ready",0));
        for (;;) {
            auto m=input->next();
            require(str(m,"protocol")==protocol && str(m,"request_id")==id && integer(m,"episode_number")==static_cast<uint64_t>(episode),"request_identity");
            auto type=str(m,"type");
            require(integer(m,"revision")==revision+1,"revision_order"); ++revision;
            if (type=="finalize") {
                keys(m,{"protocol","type","request_id","episode_number","revision","committed_through_sec"});
                require(decoder.total==expected && std::abs(number(m,"committed_through_sec")-duration)<=1e-9,"final_boundary");
                decoder.decode(true); auto r=response("final",revision);
                r["committed_through_sec"]=duration; r["items"]=decoder.hypothesis(); r["final"]=true; send(r); break;
            }
            require(type=="audio","audio_required");
            keys(m,{"protocol","type","request_id","episode_number","revision","start_sec","end_sec","sample_count","pcm"});
            auto count=integer(m,"sample_count"); require(count>0 && count<=320000,"sample_count_range");
            require(decoder.total+count<=static_cast<uint64_t>(expected),"sample_count_total");
            double end=static_cast<double>(decoder.total+count)/rate;
            require(std::abs(number(m,"start_sec")-static_cast<double>(decoder.total)/rate)<=1e-9 && std::abs(number(m,"end_sec")-end)<=1e-9,"audio_boundary");
            auto samples=pcm(str(m,"pcm"),static_cast<size_t>(count)); J previous=decoder.suffix;
            decoder.append(samples); decoder.decode(false);
            auto r=response("hypothesis",revision); r["committed_through_sec"]=end; r["items"]=decoder.hypothesis(); send(r);
            decoder.observe(previous);
        }
        result=0;
    } catch (const Failure & e) {
        try { auto r=envelope("error",id,episode,revision); r["error_code"]=e.what(); send(r); } catch (...) {}
    } catch (...) {
        try { auto r=envelope("error",id,episode,revision); r["error_code"]="message_decode"; send(r); } catch (...) {}
    }
    std::_Exit(result);
}
