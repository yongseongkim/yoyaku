#include "bridge.h"
#include <dlfcn.h>
#include <stdlib.h>
#include <stdio.h>
#include <stdatomic.h>

// 이 파일은 Go에서 whisper.cpp를 호출하고 인식을 취소하는 데 필요한 연결 코드다.
// 세션과 모델 관리, HTTP 요청 처리, 입력 검사, 통역 시점 판단은 Go에서 맡는다.
struct yoyaku_whisper {
    void *library;
    struct whisper_context *context;
    __typeof__(whisper_context_default_params) *context_default;
    __typeof__(whisper_full_default_params) *full_default;
    __typeof__(whisper_init_from_file_with_params) *init;
    __typeof__(whisper_free) *free;
    __typeof__(whisper_full) *full;
    __typeof__(whisper_full_n_segments) *segments;
    __typeof__(whisper_full_get_segment_text) *text;
    __typeof__(whisper_full_get_segment_t0) *start;
    __typeof__(whisper_full_get_segment_t1) *end;
    __typeof__(whisper_full_n_tokens) *tokens;
    __typeof__(whisper_full_get_token_text) *token_text;
    __typeof__(whisper_full_get_token_data) *token_data;
    __typeof__(whisper_token_eot) *eot;
};
struct yoyaku_cancel { atomic_bool cancelled; };
static _Thread_local char last_error[512];
const char *yoyaku_error(void) { return last_error; }
#define LOAD(member, name) do { *(void **)(&api->member) = dlsym(api->library, #name); if (!api->member) { snprintf(last_error,sizeof(last_error),"Missing native ABI: %s",#name); yoyaku_close(api); return NULL; } } while (0)
yoyaku_whisper *yoyaku_open(const char *library, const char *model) {
    yoyaku_whisper *api = calloc(1,sizeof(*api));
    if (!api) return NULL;
    api->library=dlopen(library,RTLD_NOW|RTLD_LOCAL);
    if (!api->library) { snprintf(last_error,sizeof(last_error),"%s",dlerror()); free(api); return NULL; }
    LOAD(context_default,whisper_context_default_params); LOAD(full_default,whisper_full_default_params);
    LOAD(init,whisper_init_from_file_with_params); LOAD(free,whisper_free); LOAD(full,whisper_full);
    LOAD(segments,whisper_full_n_segments); LOAD(text,whisper_full_get_segment_text);
    LOAD(start,whisper_full_get_segment_t0); LOAD(end,whisper_full_get_segment_t1);
    LOAD(tokens,whisper_full_n_tokens); LOAD(token_text,whisper_full_get_token_text);
    LOAD(token_data,whisper_full_get_token_data); LOAD(eot,whisper_token_eot);
    struct whisper_context_params params=api->context_default(); params.use_gpu=true;
    api->context=api->init(model,params);
    if (!api->context) { snprintf(last_error,sizeof(last_error),"Native Whisper model loading failed"); yoyaku_close(api); return NULL; }
    return api;
}
void yoyaku_close(yoyaku_whisper *api) { if (!api) return; if (api->context && api->free) api->free(api->context); if (api->library) dlclose(api->library); free(api); }
yoyaku_cancel *yoyaku_cancel_new(void) { return calloc(1,sizeof(yoyaku_cancel)); }
void yoyaku_cancel_set(yoyaku_cancel *flag) { atomic_store(&flag->cancelled,true); }
void yoyaku_cancel_free(yoyaku_cancel *flag) { free(flag); }
static bool cancelled(void *data) { return atomic_load(&((yoyaku_cancel *)data)->cancelled); }
int yoyaku_transcribe(yoyaku_whisper *api,const float *audio,int count,const char *lang,const char *prompt,yoyaku_cancel *flag) {
    struct whisper_full_params params=api->full_default(WHISPER_SAMPLING_GREEDY);
    params.n_threads=4; params.no_context=true; params.translate=false;
    params.language=lang; params.initial_prompt=prompt; params.detect_language=false;
    params.token_timestamps=true; params.print_progress=false; params.print_realtime=false; params.print_timestamps=false; params.print_special=false;
    params.temperature=0; params.temperature_inc=0; params.greedy.best_of=1;
    params.abort_callback=cancelled; params.abort_callback_user_data=flag;
    return api->full(api->context,params,audio,count);
}
int yoyaku_segments(yoyaku_whisper *api) { return api->segments(api->context); }
const char *yoyaku_segment_text(yoyaku_whisper *api,int s) { return api->text(api->context,s); }
int64_t yoyaku_segment_start(yoyaku_whisper *api,int s) { return api->start(api->context,s); }
int64_t yoyaku_segment_end(yoyaku_whisper *api,int s) { return api->end(api->context,s); }
int yoyaku_tokens(yoyaku_whisper *api,int s) { return api->tokens(api->context,s); }
const char *yoyaku_token_text(yoyaku_whisper *api,int s,int t) { return api->token_text(api->context,s,t); }
whisper_token_data yoyaku_token_data(yoyaku_whisper *api,int s,int t) { return api->token_data(api->context,s,t); }
int yoyaku_eot(yoyaku_whisper *api) { return api->eot(api->context); }
