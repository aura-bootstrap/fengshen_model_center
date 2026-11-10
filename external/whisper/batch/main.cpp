// Independent full-audio batch entry; reuse the production UTF-8/timeline reducer.
#pragma push_macro("main")
#undef main
#define main fengshen_stream_entry_not_called
#include "../stream/main.cpp"
#pragma pop_macro("main")
#include <filesystem>
#include <fstream>
#include <sstream>

static constexpr size_t batch_limit = 4u << 20;
static constexpr uint64_t wav_limit = 1ull << 30;
static constexpr const char * batch_version = "4979e04-batch-v2";
struct BatchArgs { std::string model, wav, output; bool vad; std::string vad_model; };
static BatchArgs batch_args(int argc, char ** argv) {
    require(argc == 17 || argc == 20, "arguments");
    for (auto pair : {std::pair<int,const char *>{1,"-m"},{3,"-f"},{5,"-l"},{6,"zh"},{7,"-t"},{8,"4"},{9,"-ml"},{10,"1"},{11,"-sow"},{12,"-ojf"},{13,"-of"},{15,"-np"},{16,"-ng"}}) {
        require(std::string(argv[pair.first]) == pair.second, "arguments");
    }
    require(argv[2][0] && argv[4][0] && argv[14][0], "arguments");
    if(argc == 20) require(std::string(argv[17]) == "--vad" && std::string(argv[18]) == "--vad-model" && argv[19][0] && std::string(argv[19]).rfind("-",0) != 0, "arguments");
    return {argv[2],argv[4],argv[14],argc == 20,argc == 20 ? argv[19] : ""};
}
static uint32_t le(const unsigned char * p, int n) {
    uint32_t v=0; for (int i=0;i<n;++i) v |= static_cast<uint32_t>(p[i]) << (8*i); return v;
}
static void exact(std::istream & in, unsigned char * p, size_t n) {
    require(static_cast<bool>(in.read(reinterpret_cast<char *>(p), static_cast<std::streamsize>(n))), "wav_invalid");
}
static std::vector<float> read_wav(std::istream & in, uint64_t size) {
    require(size >= 44 && size <= wav_limit, "wav_size");
    unsigned char h[16]; exact(in,h,12);
    require(std::string(reinterpret_cast<char *>(h),4)=="RIFF" && std::string(reinterpret_cast<char *>(h+8),4)=="WAVE" && le(h+4,4)+8ull==size, "wav_invalid");
    uint64_t pos=12; bool fmt=false, data=false; std::vector<float> samples;
    while (pos<size) {
        require(size-pos>=8,"wav_invalid"); exact(in,h,8); pos+=8;
        std::string tag(reinterpret_cast<char *>(h),4); uint64_t n=le(h+4,4), padded=n+(n&1);
        require(padded<=size-pos,"wav_invalid");
        if (tag=="fmt ") {
            require(!fmt && !data && n==16,"wav_format"); exact(in,h,16);
            require(le(h,2)==1 && le(h+2,2)==1 && le(h+4,4)==16000 && le(h+8,4)==32000 && le(h+12,2)==2 && le(h+14,2)==16,"wav_format"); fmt=true;
        } else if (tag=="data") {
            require(fmt && !data && n>0 && n%2==0 && n/2<=7200ull*rate,"wav_duration"); data=true;
            samples.resize(static_cast<size_t>(n/2)); unsigned char block[8192]; size_t offset=0;
            while (offset<samples.size()) {
                size_t count=std::min(samples.size()-offset,sizeof(block)/2); exact(in,block,count*2);
                for(size_t i=0;i<count;++i) samples[offset+i]=static_cast<int16_t>(le(block+2*i,2))/32768.f;
                offset+=count;
            }
        } else { in.seekg(static_cast<std::streamoff>(n),std::ios::cur); require(static_cast<bool>(in),"wav_invalid"); }
        if(n&1) { unsigned char pad; exact(in,&pad,1); }
        pos+=padded;
    }
    require(fmt && data && in.peek()==EOF,"wav_invalid"); return samples;
}
static int64_t milliseconds(const J & time) { return static_cast<int64_t>(std::llround(time.get<double>()*1000)); }
static void batch_segment(J & document, const J & segment, size_t & budget) {
    J tokens=J::array(); std::string text;
    for(const auto & w:segment["words"]) {
        auto piece=w["text"].get<std::string>(); auto trimmed=compact(piece);
        // These strings are control tokens to the fixed adapter, not lexical output.
        require(!trimmed.empty() && !(trimmed.rfind("<|",0)==0 && trimmed.size()>=4 && trimmed.substr(trimmed.size()-2)=="|>") && !(trimmed.rfind("[_",0)==0 && trimmed.back()==']'),"token_invalid");
        auto from=milliseconds(w["start_sec"]), to=milliseconds(w["end_sec"]); require(from<to,"token_timeline");
        require(piece.size()<=batch_limit && budget<=batch_limit-piece.size(),"output_limit"); budget+=piece.size();
        require(budget<=batch_limit-160,"output_limit"); budget+=160;
        tokens.push_back({{"text",piece},{"offsets",{{"from",from},{"to",to}}}}); text+=piece;
    }
    require(!tokens.empty(),"result_empty");
    document["transcription"].push_back({{"text",text},{"offsets",{{"from",tokens.front()["offsets"]["from"]},{"to",tokens.back()["offsets"]["to"]}}},{"tokens",tokens}});
}
static void batch_token(TokenAccumulator & tokens, const std::string & piece, int64_t t0, int64_t t1) {
    auto trimmed=compact(piece);
    if((trimmed.rfind("<|",0)==0 && trimmed.size()>=4 && trimmed.substr(trimmed.size()-2)=="|>") || (trimmed.rfind("[_",0)==0 && trimmed.back()==']')) return;
    tokens.add(piece,t0,t1);
    require(tokens.text.size()+tokens.pending.size()+tokens.fragment.size()<=batch_limit && tokens.words.size()<=batch_limit/160,"output_limit");
}
static void batch_finish(J & doc, TokenAccumulator & tokens, size_t & budget) {
    J reduced=J::array(); tokens.finish(reduced);
    // Complete lexical groups may cross upstream segment boundaries, not VAD ranges.
    for(const auto & item:reduced) for(const auto & word:item["words"]) {
        J group={{"words",J::array({word})}}; batch_segment(doc,group,budget);
    }
}
struct BoundedJSON : std::streambuf {
    std::string bytes;
    int_type overflow(int_type c) override {
        if(traits_type::eq_int_type(c,traits_type::eof())) return traits_type::not_eof(c);
        require(bytes.size()<batch_limit,"output_limit"); bytes.push_back(traits_type::to_char_type(c)); return c;
    }
    std::streamsize xsputn(const char * s,std::streamsize n) override {
        require(n>=0 && static_cast<size_t>(n)<=batch_limit-bytes.size(),"output_limit"); bytes.append(s,static_cast<size_t>(n)); return n;
    }
};
static std::string batch_json(const J & doc) {
    require(!doc["transcription"].empty(),"result_empty"); BoundedJSON buffer; std::ostream out(&buffer); out.exceptions(std::ios::badbit|std::ios::failbit); out<<doc; return buffer.bytes;
}
static void batch_write(const std::string & name,const std::string & bytes) {
#ifdef _WIN32
    auto path=std::filesystem::u8path(name).wstring();
    HANDLE f=CreateFileW(path.c_str(),GENERIC_WRITE,0,nullptr,CREATE_NEW,FILE_ATTRIBUTE_NORMAL|FILE_FLAG_OPEN_REPARSE_POINT,nullptr);
    require(f!=INVALID_HANDLE_VALUE,"output_open"); DWORD written=0;
    bool ok=WriteFile(f,bytes.data(),static_cast<DWORD>(bytes.size()),&written,nullptr)!=0 && written==bytes.size();
    bool closed=CloseHandle(f)!=0; require(ok && closed,"output_write");
#else
    (void)name; (void)bytes; throw Failure("platform_unsupported");
#endif
}
static J batch_info() {
    return {{"schema",1},{"component_id","whisper-batch"},{"version",batch_version},{"os","windows"},{"arch","amd64"},{"native",true},{"packaging","native"},{"protocol","fengshen.whispercpp.batch/v1"},{"protocols",J::array({"fengshen.whispercpp.batch/v1"})},{"source_commit","4979e04f5dcaccb36057e059bbaed8a2f5288315"},{"vad_supported",true},{"inference","not_tested"}};
}
int main(int argc,char ** argv) {
#ifdef _WIN32
    SetErrorMode(SEM_FAILCRITICALERRORS|SEM_NOGPFAULTERRORBOX);
#endif
    whisper_log_set(quiet,nullptr);
    try {
        if(argc==2 && std::string(argv[1])=="--component-info") { std::cout<<batch_info().dump()<<'\n'; return 0; }
        auto args=batch_args(argc,argv);
        std::ifstream in(std::filesystem::u8path(args.wav),std::ios::binary|std::ios::ate); require(static_cast<bool>(in),"wav_open");
        auto size=in.tellg(); require(size>=0,"wav_size"); in.seekg(0); auto audio=read_wav(in,static_cast<uint64_t>(size)); in.close();
        auto cp=whisper_context_default_params(); cp.use_gpu=false;
        std::unique_ptr<whisper_context,decltype(&whisper_free)> ctx(whisper_init_from_file_with_params(args.model.c_str(),cp),whisper_free); require(ctx!=nullptr,"model_load");
        auto p=whisper_full_default_params(WHISPER_SAMPLING_GREEDY); p.n_threads=4; p.language="zh";
        p.print_special=p.print_progress=p.print_realtime=p.print_timestamps=false;
        p.token_timestamps=true; p.max_len=1; p.split_on_word=true; p.vad=false;
        std::unique_ptr<whisper_vad_context,decltype(&whisper_vad_free)> vad(nullptr,whisper_vad_free);
        std::unique_ptr<whisper_state,decltype(&whisper_free_state)> state(nullptr,whisper_free_state);
        std::vector<VADRange> ranges;
        if(args.vad) {
            auto vp=whisper_vad_default_context_params(); vp.use_gpu=false; vp.n_threads=4;
            vad.reset(whisper_vad_init_from_file_with_params(args.vad_model.c_str(),vp)); require(vad!=nullptr,"vad_model_load");
            std::unique_ptr<whisper_vad_segments,decltype(&whisper_vad_free_segments)> segments(
                whisper_vad_segments_from_samples(vad.get(),whisper_vad_default_params(),audio.data(),static_cast<int>(audio.size())),whisper_vad_free_segments);
            require(segments!=nullptr,"vad_failed"); int64_t end=0;
            for(int i=0;i<whisper_vad_segments_n_segments(segments.get());++i) {
                auto range=vad_range(whisper_vad_segments_get_segment_t0(segments.get(),i),whisper_vad_segments_get_segment_t1(segments.get(),i),audio.size(),end);
                if(range.end>range.start) ranges.push_back(range);
                end=range.end;
            }
            require(!ranges.empty(),"result_empty");
            state.reset(whisper_init_state(ctx.get())); require(state!=nullptr,"state_init");
            p.no_context=true;
        } else ranges.push_back({0,static_cast<int64_t>(audio.size())});
        J doc={{"transcription",J::array()}}; double candidate_end=0; size_t budget=32;
        for(auto range:ranges) {
            int decoded=args.vad ? whisper_full_with_state(ctx.get(),state.get(),p,audio.data()+range.start,static_cast<int>(range.end-range.start))
                                 : whisper_full(ctx.get(),p,audio.data(),static_cast<int>(audio.size()));
            require(decoded==0,"decode_failed");
            TokenAccumulator tokens{J::array(),"","",-1,-1,args.vad,range,0,static_cast<int64_t>(audio.size()),0,candidate_end};
            int count=args.vad ? whisper_full_n_segments_from_state(state.get()) : whisper_full_n_segments(ctx.get());
            for(int s=0;s<count;++s) {
                int n=args.vad ? whisper_full_n_tokens_from_state(state.get(),s) : whisper_full_n_tokens(ctx.get(),s);
                for(int t=0;t<n;++t) {
                    auto td=args.vad ? whisper_full_get_token_data_from_state(state.get(),s,t) : whisper_full_get_token_data(ctx.get(),s,t);
                    if(td.id>=whisper_token_eot(ctx.get())) continue;
                    auto piece=args.vad ? whisper_full_get_token_text_from_state(ctx.get(),state.get(),s,t) : whisper_full_get_token_text(ctx.get(),s,t);
                    batch_token(tokens,piece,td.t0,td.t1);
                }
            }
            batch_finish(doc,tokens,budget);
        }
        batch_write(args.output+".json",batch_json(doc)); return 0;
    } catch(const Failure & e) { std::fprintf(stderr,"%s\n",e.what()); }
      catch(...) { std::fputs("batch_failed\n",stderr); }
    return 1;
}
